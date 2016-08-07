package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/groob/plist"
	"github.com/juju/deputy"
)

// Config autopkgd config
type Config struct {
	AutopkgCmdPath      string        `toml:"autopkg_path,omitempty"`
	MakecatalogsCmdPath string        `toml:"makecatalogs_path,omitempty"`
	RecipesFile         string        `toml:"recipes_file"`
	MunkiRepoPath       string        `toml:"munki_repo"`
	ReportsPath         string        `toml:"reports_path"`
	MaxProcesses        int           `toml:"max_processes"`
	ExecTimeout         time.Duration `toml:"autopkg_exec_timeout"`
	CheckInterval       time.Duration `toml:"autopkg_check_interval"`

	// Slack config
	Slack slack `toml:"slack"`
}

type processor struct {
	DataRows    []map[string]interface{} `plist:"data_rows"`
	Header      []string                 `plist:"header"`
	SummaryText string                   `plist:"summary_text"`
}

type autopkgReport struct {
	Failures       []interface{}        `plist:"failures"`
	SummaryResults map[string]processor `plist:"summary_results"`
}

func runAutopkg(recipe, reportsPath, cmdPath string, check bool, execTimeout time.Duration) autopkgReport {
	autopkgCmd := exec.Command(cmdPath, "run", "--report-plist="+reportsPath+"/"+recipe)

	if check {
		autopkgCmd.Args = append(autopkgCmd.Args, "--check")
	}

	autopkgCmd.Args = append(autopkgCmd.Args, recipe)
	d := deputy.Deputy{
		Errors:    deputy.FromStderr,
		StdoutLog: func(b []byte) { log.Print(string(b)) },
		Timeout:   time.Second * execTimeout,
	}
	if err := d.Run(autopkgCmd); err != nil {
		log.Println(err)
		return autopkgReport{}
	}
	report, err := readReportPlist(reportsPath + "/" + recipe)
	if err != nil {
		log.Println(err)
		return autopkgReport{}
	}
	return report
}

func readReportPlist(path string) (autopkgReport, error) {
	r := autopkgReport{}
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	return r, plist.NewDecoder(f).Decode(&r)
}

func makeCatalogs(makeCatalogsPath, repoPath string, execTimeout time.Duration) {
	makecatalogsCmd := exec.Command(
		makeCatalogsPath,
		repoPath,
	)
	d := deputy.Deputy{
		Errors:    deputy.FromStderr,
		StdoutLog: func(b []byte) { log.Println(string(b)) },
		Timeout:   time.Second * execTimeout,
	}
	if err := d.Run(makecatalogsCmd); err != nil {
		log.Println(err)
		return
	}
}

func process(done chan<- bool, concurrency int, slackReport, check bool, recipeFile, autopkgCmdPath, makecatalogsPath, repoPath, reportsPath string, execTimeout time.Duration, slackConfig slack) {
	var catalogsModified bool
	sem := make(chan int, concurrency)

	// make a channel of autopkgReports and create workers
	// close the reports channel when done
	reports := make(chan autopkgReport)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		wg.Wait()
		close(reports)
	}()

	// create a channel of recipes for each worker to run
	recipes := make(chan string)
	go func() {
		defer wg.Done()
		file, err := os.Open(recipeFile)
		if err != nil {
			log.Println(err)
			return
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			recipe := scanner.Text()
			// ignore empty lines, comments and MakeCatalogs.munki
			if len(recipe) == 0 || recipe == "MakeCatalogs.munki" || []byte(recipe)[0] == []byte("#")[0] {
				continue
			}
			recipes <- recipe
		}
		close(recipes)
	}()

	// Send reports to slack if flag is enabled
	if slackReport {
		go notifySlack(reports, slackConfig)
	}

	go func() {
		for report := range reports {
			if _, ok := report.SummaryResults["munki_importer_summary_result"]; ok {
				catalogsModified = true
			}
		}
	}()

	for recipe := range recipes {
		wg.Add(1)
		sem <- 1
		go func(recipe string) {
			reports <- runAutopkg(recipe, reportsPath, autopkgCmdPath, check, execTimeout)
			wg.Done()
			<-sem
		}(recipe)
	}

	if catalogsModified {
		makeCatalogs(makecatalogsPath, repoPath, execTimeout)
	}

	done <- true
}

func main() {
	var (
		conf     Config
		fConfig  = flag.String("config", "", "configuration file to load")
		fSlack   = flag.Bool("slack", false, "Send reports to slack?")
		fCheck   = flag.Bool("check", false, "autopkg check option")
		fVersion = flag.Bool("version", false, "display the version")
		// Version string
		Version = "unreleased"
	)
	flag.Parse()

	if *fVersion {
		fmt.Printf("autopkgd - version %s\n", Version)
		os.Exit(0)
	}

	if _, err := toml.DecodeFile(*fConfig, &conf); err != nil {
		log.Fatal(err)
	}

	if conf.AutopkgCmdPath == "" {
		conf.AutopkgCmdPath = "/usr/local/bin/autopkg"
	}

	if conf.MakecatalogsCmdPath == "" {
		conf.MakecatalogsCmdPath = "/usr/local/munki/makecatalogs"
	}

	if conf.MaxProcesses == 0 {
		conf.MaxProcesses = 1
	}

	if conf.ExecTimeout == 0 {
		conf.ExecTimeout = 600
	}

	if conf.CheckInterval == 0 {
		conf.CheckInterval = 1
	}

	// is report path configured?
	if conf.ReportsPath == "" {
		fmt.Println("You must specify a directory for reports to be saved in your config")
		os.Exit(1)
	}

	// does report path exist?
	fileInfo, err := os.Stat(conf.ReportsPath)
	if os.IsNotExist(err) {
		fmt.Printf("No such file or directory: %s\n", conf.ReportsPath)
		os.Exit(1)
	}

	if !fileInfo.IsDir() {
		fmt.Printf("%v must be a directory\n", conf.ReportsPath)
		os.Exit(1)
	}

	// loop through all the recipes at an interval
	// done blocks untill process finishes
	done := make(chan bool)
	ticker := time.NewTicker(time.Second * conf.CheckInterval).C
	for {
		go process(done, conf.MaxProcesses, *fSlack, *fCheck, conf.RecipesFile, conf.AutopkgCmdPath, conf.MakecatalogsCmdPath, conf.ReportsPath, conf.ReportsPath, conf.ExecTimeout, conf.Slack)
		<-done
		<-ticker
	}
}
