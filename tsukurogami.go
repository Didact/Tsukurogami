package main

import (
	"log"
	"fmt"
	"crypto/tls"
	"flag"
	"net/http"
	"strings"
	"encoding/base64"
)

// flags
var (
	xcodeURL = flag.String("xcode-url", "", "The url of your xcode server")
	bitbucketURL = flag.String("bitbucket-url", "", "The url of your bitbucket server")
	xcodeCredentials = flag.String("xcode-credentials", "", "The credentials for your xcode server. username:password")
	bitbucketCredentials = flag.String("bitbucket-credentials", "", "The credentials for your bitbucket server. username:password")
	port = flag.Int("port", 4444, "The port to listen on")
	// configFilePath = flag.String("config", "", "(Optional) Path to configuration file")
	skipVerify = flag.Bool("skip-verify", true, "Skip certification verification on the xcode server")
	template = flag.String("template", "$REPO_NAME.continuous", "The bot from which settings should be copied")
)

var client *http.Client

type transport struct {
	http.Transport
	creds string
}

func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	auth := fmt.Sprintf("Basic %s", base64.StdEncoding.EncodeToString([]byte(t.creds)))
	req.Header.Add("Authorization", auth)
	return t.Transport.RoundTrip(req)
}

func handlePullRequestUpdated(w http.ResponseWriter, r *http.Request) {

	repo, ok := r.URL.Query()["repo"]
	if !ok || len(repo) < 1 {
		w.WriteHeader(400)
		w.Write([]byte(`missing "repo" parameter`))
		return
	}

	branch, ok := r.URL.Query()["branch"]
	if !ok || len(branch) < 1 {
		w.WriteHeader(400)
		w.Write([]byte(`missing "branch" parameter`))
		return
	}

	status, ok := r.URL.Query()["status"]
	if !ok || len(status) < 1 {
		w.WriteHeader(400)
		w.Write([]byte(`missing "status" parameter`))
		return
	}

	switch strings.ToLower(status[0]) {
	case "opened", "reopened":
		err := createBot(repo[0], branch[0])
		w.WriteHeader(500)
		fmt.Fprintf(w, "error creating bot: %s\n", err)
	case "closed", "declined":
		err := deleteBot(repo[0], branch[0])
		w.WriteHeader(500)
		fmt.Fprintf(w, "error deleting bot: %s\n", err)
	default:
		// nop
	}

	w.WriteHeader(201)
}

func createBot(repo, branch string) error {
	return nil
}

func deleteBot(repo, branch string) error {
	return nil
}

func main() {
	flag.Parse()
	client = &http.Client{Transport: transport{creds: *xcodeCredentials, Transport: http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: *skipVerify}}}}

	http.HandleFunc("/pullRequestUpdated", handlePullRequestUpdated)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}
