package cmd

import (
	"fmt"
	"os"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"sync"
	"net/http"
	"time"
	"io/ioutil"
	"net/url"
	"strconv"
	"os/exec"
	"strings"
	"regexp"
	"errors"
	"runtime"
)

var version = "1.6.0"
var cfgFile string
var userAgent string

// Flags that are either global or used in multiple commands
var apiKey string
var debugLog string
var dev bool
var hostname string
var pingApiKey string
var verbose bool
var noStdoutPassthru bool

var shortDescription = fmt.Sprintf("CronitorCLI version %s", version)

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:   "cronitor",
	Short: shortDescription,
	Long:  shortDescription + `

Command line tools for Cronitor.io. See https://cronitor.io/docs/using-cronitor-cli for details.`,
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := RootCmd.Execute(); err != nil {
		fatal(err.Error(), 1)
	}
}

var varApiKey = "CRONITOR_API_KEY"
var varHostname = "CRONITOR_HOSTNAME"
var varLog = "CRONITOR_LOG"
var varPingApiKey = "CRONITOR_PING_API_KEY"
var varExcludeText = "CRONITOR_EXCLUDE_TEXT"
var varConfig = "CRONITOR_CONFIG"

func init() {
	userAgent = fmt.Sprintf("CronitorCLI/%s", version)
	cobra.OnInitialize(initConfig)

	// Here you will define your flags and configuration settings.
	// Cobra supports persistent flags, which, if defined here,
	// will be global for your application.
	RootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", cfgFile, "Config file (default: .cronitor.json)")
	RootCmd.PersistentFlags().StringVarP(&apiKey, "api-key", "k", apiKey, "Cronitor API Key")
	RootCmd.PersistentFlags().StringVarP(&pingApiKey, "ping-api-key", "p", pingApiKey, "Ping API Key")
	RootCmd.PersistentFlags().StringVarP(&hostname, "hostname", "n", hostname, "A unique identifier for this host (default: system hostname)")
	RootCmd.PersistentFlags().StringVarP(&debugLog, "log", "l", debugLog, "Write debug logs to supplied file")
	RootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", verbose, "Verbose output")

	RootCmd.PersistentFlags().BoolVar(&dev, "use-dev", dev, "Dev mode")
	RootCmd.PersistentFlags().MarkHidden("use-dev")

	viper.BindPFlag(varApiKey, RootCmd.PersistentFlags().Lookup("api-key"))
	viper.BindPFlag(varHostname, RootCmd.PersistentFlags().Lookup("hostname"))
	viper.BindPFlag(varLog, RootCmd.PersistentFlags().Lookup("log"))
	viper.BindPFlag(varPingApiKey, RootCmd.PersistentFlags().Lookup("ping-api-key"))
	viper.BindPFlag(varConfig, RootCmd.PersistentFlags().Lookup("config"))
}

// initConfig reads in config file and ENV variables if set.
func initConfig() {

	viper.AutomaticEnv() // read in environment variables that match

	// If a custom config file is specified by flag or env var, use it. Otherwise use default file.
	if len(viper.GetString(varConfig)) > 0 {
		viper.SetConfigFile(viper.GetString(varConfig))
	} else {
		// Search config in home directory
		viper.AddConfigPath(defaultConfigFileDirectory())
		viper.SetConfigName("cronitor")
	}


	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err == nil {
		log("Reading config from " + viper.ConfigFileUsed())
	}
}

func sendPing(endpoint string, uniqueIdentifier string, message string, tag string, timestamp float64, duration *float64, exitCode *int, group *sync.WaitGroup) {
	defer group.Done()

	Client := &http.Client{
		Timeout: time.Second * 3,
	}

	pingApiAuthKey := viper.GetString(varPingApiKey)
	hostname := effectiveHostname()
	formattedStamp := ""
	formattedDuration := ""
	formattedStatusCode := ""

	if timestamp > 0 {
		formattedStamp = fmt.Sprintf("&stamp=%s", formatStamp(timestamp))
	}

	if len(message) > 0 {
		message = fmt.Sprintf("&msg=%s", url.QueryEscape(truncateString(message, 2000)))
	}

	if len(pingApiAuthKey) > 0 {
		pingApiAuthKey = fmt.Sprintf("&auth_key=%s", truncateString(pingApiAuthKey, 50))
	}

	if len(hostname) > 0 {
		hostname = fmt.Sprintf("&host=%s", url.QueryEscape(truncateString(hostname, 50)))
	}

	// By passing duration up, we save the computation on the server side
	if duration != nil {
		formattedDuration = fmt.Sprintf("&duration=%s", formatStamp(*duration))
	}

	// We aren't using exit code at time of writing, but we have the field available for healthcheck monitors.
	if exitCode != nil {
		formattedStatusCode = fmt.Sprintf("&status_code=%d", *exitCode)
	}

	// The `tag` data is used to match start events and run events. Useful if multiple instances of a job are running.
	if len(tag) > 0 {
		tag = fmt.Sprintf("&tag=%s", tag)
	}

	for i := 1; i <= 6; i++ {
		// Determine the ping API host. After a few failed attempts, try using cronitor.io instead
		var host string
		if dev {
			host = "http://dev.cronitor.io"
		} else if i > 2 && host == "https://cronitor.link" {
			host = "https://cronitor.io"
		} else {
			host = "https://cronitor.link"
		}

		uri := fmt.Sprintf("%s/%s/%s?try=%d%s%s%s%s%s%s%s", host, uniqueIdentifier, endpoint, i, formattedStamp, message, pingApiAuthKey, hostname, formattedDuration, tag, formattedStatusCode)
		log("Sending ping " + uri)

		request, err := http.NewRequest("GET", uri, nil)
		request.Header.Add("User-Agent", userAgent)
		response, err := Client.Do(request)

		if err != nil {
			log(err.Error())

			// After 3 failed attempts, begin to sleep between tries
			if i > 2 {
				time.Sleep(time.Second * 2)
			}
			continue
		}

		_, err = ioutil.ReadAll(response.Body)
		if err == nil && response.StatusCode < 400 {
			break
		}

		response.Body.Close()
	}
}

func sendApiRequest(url string) ([]byte, error) {
	client := &http.Client{}
	request, err := http.NewRequest("GET", url, nil)
	request.SetBasicAuth(viper.GetString(varApiKey), "")
	request.Header.Add("Content-Type", "application/json")
	request.Header.Add("User-Agent", userAgent)
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}

	if response.StatusCode != 200 {
		return nil, errors.New(fmt.Sprintf("Unexpected %d API response", response.StatusCode))
	}

	defer response.Body.Close()
	contents, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}

	return contents, nil
}

func effectiveHostname() string {
	if len(viper.GetString(varHostname)) > 0 {
		return viper.GetString(varHostname)
	}

	hostname, _ := os.Hostname()
	return hostname
}

func timezoneLocationName() string {
	// First, check a TZ environemnt variable if one is set
	if locale, isSetFlag := os.LookupEnv("TZ"); isSetFlag {
		return locale
	}

	// Attempt to parse timedatectl (should work on FreeBSD, many linux distros)
	if output, err := exec.Command("timedatectl").Output(); err == nil {
		outputString := strings.Replace(string(output), "Time zone", "Timezone", -1)
		r := regexp.MustCompile(`(?m:Timezone:\s+(\S+).+$)`)
		if ret := r.FindStringSubmatch(outputString); ret != nil && len(ret) > 1 {
			return ret[1]
		}
	}

	// If /etc/localtime is a symlink, check what it is linking to
	if localtimeFile, err := os.Lstat("/etc/localtime"); err == nil && localtimeFile.Mode() & os.ModeSymlink == os.ModeSymlink {
		if symlink, _ := os.Readlink("/etc/localtime"); len(symlink) > 0 {
			symlinkParts := strings.Split(symlink, "/")
			return strings.Join(symlinkParts[len(symlinkParts)-2:], "/")
		}
	}

	// If we happen to have an /etc/timezone, no guarantee it's used, but read that
	if locale, err := ioutil.ReadFile("/etc/timezone"); err == nil {
		return string(locale)
	}

	return ""
}

func apiUrl() string {
	if dev {
		return "http://dev.cronitor.io/v3/monitors"
	} else {
		return "https://cronitor.io/v3/monitors"
	}
}

func defaultConfigFileDirectory() string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("%s\\ProgramData\\Cronitor", os.Getenv("SYSTEMDRIVE"))
	}

	return "/etc/cronitor"
}

func truncateString(s string, length int) string {
	if len(s) <= length {
		return s
	}

	return s[:length]
}

func log(msg string) {
	debugLog := viper.GetString(varLog)
	if len(debugLog) > 0 {
		f, _ := os.OpenFile(debugLog, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
		defer f.Close()
		f.WriteString(msg + "\n")
	}

	if verbose {
		fmt.Println(msg)
	}
}

func fatal(msg string, exitCode int) {
	debugLog := viper.GetString(varLog)
	if len(debugLog) > 0 {
		f, _ := os.OpenFile(debugLog, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
		defer f.Close()
		f.WriteString(msg + "\n")
	}

	fmt.Fprintln(os.Stderr, msg)
	os.Exit(exitCode)
}

func makeStamp() float64 {
	return float64(time.Now().UnixNano()) / float64(time.Second)
}

func formatStamp(timestamp float64) string {
	return strconv.FormatFloat(timestamp, 'f', 3, 64)
}
