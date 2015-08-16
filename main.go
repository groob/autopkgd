package main

import (
	"bufio"
	"flag"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/juju/deputy"
	"howett.net/plist"
)

var (
	wg      sync.WaitGroup
	conf    Config
	fConfig = flag.String("config", "", "configuration file to load")
	fSlack  = flag.Bool("slack", false, "Send reports to slack?")
	fCheck  = flag.Bool("check", false, "autopkg check option")
)

// autopkgd config
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

// read each AutoPkg recipe from a text file and
// send on a channel.
// close channel when done.
func readRecipeList(recipes chan string) {
	defer wg.Done()
	file, err := os.Open(conf.RecipesFile)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		recipe := scanner.Text()
		// ignore empty lines, comments and MakeCatalogs.munki
		if len(recipe) == 0 ||
			recipe == "MakeCatalogs.munki" ||
			[]byte(recipe)[0] == []byte("#")[0] {
			continue
		}
		recipes <- recipe
	}
	close(recipes)
}

func runAutopkg(recipe string) *autopkgReport {
	autopkgCmd := exec.Command(conf.AutopkgCmdPath, "run", "--report-plist="+conf.ReportsPath+"/"+recipe)

	if *fCheck {
		autopkgCmd.Args = append(autopkgCmd.Args, "--check")
	}

	autopkgCmd.Args = append(autopkgCmd.Args, recipe)
	d := deputy.Deputy{
		Errors:    deputy.FromStderr,
		StdoutLog: func(b []byte) { log.Print(string(b)) },
		Timeout:   time.Second * conf.ExecTimeout,
	}
	if err := d.Run(autopkgCmd); err != nil {
		log.Print(err)
	}
	report, err := readReportPlist(conf.ReportsPath + "/" + recipe)
	if err != nil {
		log.Fatal(err)
	}
	return report
}

func readReportPlist(path string) (*autopkgReport, error) {
	r := &autopkgReport{}
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	return r, plist.NewDecoder(f).Decode(r)
}

func makeCatalogs() {
	makecatalogsCmd := exec.Command(conf.MakecatalogsCmdPath,
		conf.MunkiRepoPath)
	d := deputy.Deputy{
		Errors:    deputy.FromStderr,
		StdoutLog: func(b []byte) { log.Print(string(b)) },
		Timeout:   time.Second * conf.ExecTimeout,
	}
	if err := d.Run(makecatalogsCmd); err != nil {
		log.Print(err)
	}
}

func process(done chan bool) {
	var catalogsModified bool
	recipes := make(chan string)
	reports := make(chan *autopkgReport)
	sem := make(chan int, conf.MaxProcesses)

	wg.Add(1)
	go readRecipeList(recipes)

	// Send reports to slack if flag is enabled
	if *fSlack {
		go notifySlack(reports)
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
			reports <- runAutopkg(recipe)
			wg.Done()
			<-sem
		}(recipe)
	}

	wg.Wait()
	close(reports)

	if catalogsModified {
		makeCatalogs()
	}

	done <- true

}

func autopkgInfoHandler(w http.ResponseWriter, r *http.Request) {
	msg := &slackMsg{
		Channel:  conf.Slack.Channel,
		Username: conf.Slack.Username,
		Parse:    "full",
		IconUrl:  conf.Slack.IconUrl,
	}
	recipe := r.URL.Path[len("/info/"):]
	autopkgCmd := exec.Command(conf.AutopkgCmdPath, "info", recipe)
	output, err := autopkgCmd.Output()
	if err != nil {
		msg.Text = "```\n" + err.Error() + "\n```"
	}
	msg.Text = "```\n" + recipe + "\n" + string(output) + "\n```"
	err = msg.Post(conf.Slack.WebhookUrl)
	if err != nil {
		log.Println(err)
	}
}

func autopkgRunHandler(w http.ResponseWriter, r *http.Request) {
	var catalogsModified bool
	recipe := r.URL.Path[len("/run/"):]
	reports := make(chan *autopkgReport)

	if *fSlack {
		go notifySlack(reports)
	}

	go func() {
		for report := range reports {
			if _, ok := report.SummaryResults["munki_importer_summary_result"]; ok {
				catalogsModified = true
			}
		}
	}()

	reports <- runAutopkg(recipe)

	if catalogsModified {
		makeCatalogs()
	}

	close(reports)
}

func serve() {
	http.HandleFunc("/run/", autopkgRunHandler)
	http.HandleFunc("/info/", autopkgInfoHandler)
	log.Fatal(http.ListenAndServe(":8881", nil))
}

func init() {
	flag.Parse()
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

}

func main() {
	go serve()
	done := make(chan bool)
	ticker := time.NewTicker(time.Second * conf.CheckInterval).C
	for {
		go process(done)
		<-done
		<-ticker
	}
}
