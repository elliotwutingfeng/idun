package crawler

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	sigar "github.com/cloudfoundry/gosigar"
	"github.com/gocolly/colly/v2"
	"github.com/gocolly/colly/v2/debug"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/tb0hdan/idun/pkg/client"
	"github.com/tb0hdan/idun/pkg/robots"
	"github.com/tb0hdan/idun/pkg/utils"
	"github.com/tb0hdan/idun/pkg/utils2"
	"github.com/tb0hdan/idun/pkg/varstruct"
)

var (
	BannedExtensions = []string{ // nolint:gochecknoglobals
		"asc", "avi", "bmp", "dll", "doc", "docx", "exe", "iso", "jpg", "mp3", "odt",
		"pdf", "png", "rar", "rdf", "svg", "tar", "tar.gz", "tar.bz2", "tgz", "txt",
		"wav", "wmv", "xml", "xz", "zip",
	}

	BannedLocalRedirects = map[string]string{ // nolint:gochecknoglobals
		"www.president.gov.ua": "1",
	}

	IgnoreNoFollow = map[string]string{ // nolint:gochecknoglobals
		"tumblr.com": "1",
	}
)

func SubmitOutgoingDomains(c *client.Client, domains []string, serverAddr string) {
	log.Println("Submit called: ", domains)
	//
	if len(domains) == 0 {
		return
	}

	var domainsRequest varstruct.DomainsResponse

	domainsRequest.Domains = utils2.DeduplicateSlice(domains)
	body, err := json.Marshal(&domainsRequest)
	//
	if err != nil {
		log.Error(err)

		return
	}

	serverURL := fmt.Sprintf("http://%s/upload", serverAddr)
	retryClient := client.PrepareClient(c.Logger)
	req, err := retryablehttp.NewRequest(http.MethodPost, serverURL, body)
	//
	if err != nil {
		log.Error(err)

		return
	}
	//
	resp, err := retryClient.Do(req)
	//
	if err != nil {
		log.Error(err)

		return
	}
	defer resp.Body.Close()
	data, err := ioutil.ReadAll(resp.Body)
	//
	if err != nil {
		log.Error(err)

		return
	}

	if resp.StatusCode != http.StatusOK {
		log.Error(string(data))
	}
}

func GetUA(reqURL string, logger *log.Logger) (string, error) {
	req, err := retryablehttp.NewRequest(http.MethodGet, reqURL, nil)
	//
	if err != nil {
		return "", err
	}
	//
	// req.Header.Add("X-Session-Token", c.Key)
	//
	retryClient := client.PrepareClient(logger)
	resp, err := retryClient.Do(req)
	//
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	message := &varstruct.JSONResponse{}
	err = json.NewDecoder(resp.Body).Decode(message)

	if err != nil {
		return "", err
	}

	if message.Code != http.StatusOK {
		return "", errors.New("non-ok response")
	}
	//
	log.Println("UA: ", message.Message)

	return message.Message, nil
}

func FilterAndSubmit(domainMap map[string]bool, c *client.Client, serverAddr string) {
	domains := make([]string, 0, len(domainMap))

	// Be nice on server and skip non-resolvable domains
	for domain := range domainMap {
		addrs, err := net.LookupHost(domain)
		//
		if err != nil {
			continue
		}
		//
		if len(addrs) == 0 {
			continue
		}
		// Local filter. Some ISPs have redirects / links to policies for blocked sites
		if _, banned := BannedLocalRedirects[domain]; banned {
			continue
		}
		//
		domains = append(domains, domain)
	}

	// At this point in time domain list can be empty (broken, banned domains)
	if len(domains) == 0 {
		return
	}

	outgoing, err := c.FilterDomains(domains)
	if err != nil {
		log.Println("Filter failed with", err)
		time.Sleep(varstruct.CrawlFilterRetry)

		return
	}

	// Don't crawl non-responsive domains (launching subprocess is expensive!)
	ua, err := c.GetUA()
	if err != nil {
		log.Println("Could not get UA: ", err.Error())

		return
	}

	checked := utils.HeadCheckDomains(outgoing, ua)
	toSubmit := make([]string, 0)

	for domain, okToSubmit := range checked {
		if !okToSubmit {
			continue
		}

		toSubmit = append(toSubmit, domain)
	}

	if len(toSubmit) == 0 {
		return
	}

	SubmitOutgoingDomains(c, toSubmit, serverAddr)
}

func CrawlURL(crawlerClient *client.Client, targetURL string, debugMode bool, serverAddr string) { // nolint:funlen,gocognit
	if len(targetURL) == 0 {
		panic("Cannot start with empty url")
	}

	if !strings.HasPrefix(targetURL, "http") {
		targetURL = fmt.Sprintf("http://%s", targetURL)
	}
	// Self-checks
	mem := sigar.Mem{}
	err := mem.Get()
	//
	if err != nil {
		panic(err)
	}

	if mem.Total < varstruct.HalfGig || mem.Free < varstruct.HalfGig {
		panic("Will not start without enough RAM. At least 512M free is required")
	}
	//
	parsed, err := url.Parse(targetURL)
	allowedDomain := strings.ToLower(parsed.Host)

	if err != nil {
		panic(err)
	}

	done := make(chan bool)

	ua, err := GetUA(fmt.Sprintf("http://%s/ua", serverAddr), crawlerClient.Logger)
	if err != nil {
		panic(err)
	}

	filters := make([]*regexp.Regexp, 0, len(BannedExtensions))
	for _, reg := range BannedExtensions {
		filters = append(filters, regexp.MustCompile(fmt.Sprintf(`.+\.%s$`, reg)))
	}

	defaultOptions := []colly.CollectorOption{
		colly.Async(true),
		colly.UserAgent(ua),
		colly.DisallowedURLFilters(filters...),
	}

	if debugMode {
		defaultOptions = append(defaultOptions, colly.Debugger(&debug.LogDebugger{}))
	}

	robo, err := robots.NewRoboTester(targetURL, ua)
	if err != nil {
		panic(err)
	}

	log.Info("CrawlDelay: ", robo.GetDelay())

	c := colly.NewCollector(
		defaultOptions...,
	)

	c.WithTransport(&http.Transport{
		DisableKeepAlives: true,
	})

	_ = c.Limit(&colly.LimitRule{
		Parallelism: varstruct.Parallelism,
		// Delay is the duration to wait before creating a new request to the matching domains
		Delay: robo.GetDelay(),
		// RandomDelay is the extra randomized duration to wait added to Delay before creating a new request
		RandomDelay: varstruct.RandomDelay,
	})

	domainMap := make(map[string]bool)

	c.OnHTML("a[href]", func(e *colly.HTMLElement) {
		link := e.Attr("href")
		absolute := e.Request.AbsoluteURL(link)

		parsed, err := url.Parse(absolute)
		parsedHost := strings.ToLower(parsed.Host)
		if err != nil {
			print(err)

			return
		}
		//
		if !strings.HasPrefix(absolute, "http") {
			return
		}
		// No follow check
		if strings.ToLower(e.Attr("rel")) == "nofollow" {
			// check ignore map
			ignore := false
			for ending := range IgnoreNoFollow {
				if strings.HasSuffix(parsedHost, ending) {
					ignore = true

					break
				}
			}

			if !ignore {
				log.Printf("Nofollow: %s\n", absolute)

				return
			}
			log.Printf("Nofollow ignored: %s\n", absolute)
		}
		//

		if !strings.HasSuffix(parsedHost, allowedDomain) {
			// external links
			if len(domainMap) < varstruct.MaxDomainsInMap {
				if _, ok := domainMap[parsedHost]; !ok {
					domainMap[parsedHost] = true
				}

				return
			}
			//
			FilterAndSubmit(domainMap, crawlerClient, serverAddr)
			//
			domainMap = make(map[string]bool)

			return
		}

		if !robo.Test(link) {
			log.Errorf("Crawling of %s is disallowed by robots.txt", absolute)

			return
		}

		_ = c.Visit(absolute)
	})

	c.OnRequest(func(r *colly.Request) {
		if debugMode {
			log.Println("Visiting", r.URL.String())
		}
	})

	// catch SIGINT / SIGTERM / SIGQUIT signals & request exit
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
		<-sig
		done <- true
	}()

	ts := time.Now()
	ticker := time.NewTicker(varstruct.TickEvery)

	go func() {
		for t := range ticker.C {
			mem := sigar.ProcMem{}
			err := mem.Get(os.Getpid())
			//
			if err != nil {
				// something's very wrong
				log.Error(err)
				done <- true

				break
			}

			log.Println("Tick at", t, mem.Resident/varstruct.OneGig)
			runtime.GC()

			if mem.Resident > varstruct.TwoGigs {
				// 2Gb MAX
				log.Println("2Gb RAM limit exceeded, exiting...")
				done <- true

				break
			}

			if t.After(ts.Add(varstruct.CrawlerMaxRunTime)) {
				log.Println("Max run time exceeded, exiting...")
				done <- true

				break
			}
		}
	}()

	if !robo.Test("/") {
		log.Errorf("Crawling of / for %s is disallowed by robots.txt", targetURL)

		return
	}

	// this one has to be started *AFTER* calling c.Visit()
	go func() {
		_ = c.Visit(targetURL)
		c.Wait()
		done <- true
	}()

	<-done
	// Submit remaining data
	FilterAndSubmit(domainMap, crawlerClient, serverAddr)
	ticker.Stop()
	log.Println("Crawler exit")
}