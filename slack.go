package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
)

type slack struct {
	WebhookURL string `toml:"webhook_url"`
	Channel    string `toml:"channel"`
	Username   string `toml:"username"`
	IconURL    string `toml:"icon_url"`
}

type slackMsg struct {
	Channel  string `json:"channel"`
	Username string `json:"username,omitempty"`
	Text     string `json:"text"`
	Parse    string `json:"parse"`
	IconURL  string `json:"icon_url,omitempty"`
}

func (m slackMsg) Encode() (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (m slackMsg) Post(WebhookURL string) error {
	encoded, err := m.Encode()
	if err != nil {
		return err
	}

	resp, err := http.PostForm(WebhookURL, url.Values{"payload": {encoded}})
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return errors.New("Not OK")
	}
	return nil
}

func notifySlack(reports <-chan autopkgReport, conf slack) {
	msg := &slackMsg{
		Channel:  conf.Channel,
		Username: conf.Username,
		Parse:    "full",
		IconURL:  conf.IconURL,
	}

	for report := range reports {
		if summary, ok := report.SummaryResults["url_downloader_summary_result"]; ok {
			for _, row := range summary.DataRows {
				downloaded := filepath.Base(row["download_path"].(string))
				msg.Text = "New download: " + downloaded
				err := msg.Post(conf.WebhookURL)
				if err != nil {
					log.Println(err)
					return
				}
			}
		}

		if summary, ok := report.SummaryResults["munki_importer_summary_result"]; ok {
			for _, row := range summary.DataRows {
				name := row["name"].(string)
				version := row["version"].(string)
				msg.Text = "New munki import: " + name + " " + version
				err := msg.Post(conf.WebhookURL)
				if err != nil {
					log.Println(err)
					return
				}
			}
		}
	}

}
