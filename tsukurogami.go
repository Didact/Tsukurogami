package main

import (
	"flag"
)

var xcodeURL = flag.String("xcode-url", "", "The url of your xcode server")
var bitbucketURL = flag.String("bitbucket-url", "", "The url of your bitbucket server")
var xcodeCredentials = flag.String("xcode-credentials", "", "The credentials for your xcode server. username:password")
var bitbucketCredentials = flag.String("bitbucket-credentials", "", "The credentials for your bitbucket server. username:password")
var port = flag.Int("port", 4444, "The port to listen on")
var configFilePath = flag.String("config", "", "(Optional) Path to configuration file")

func main() {

}
