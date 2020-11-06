package main

import (
	"bufio"
	"context"
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
	"github.com/tb0hdan/memcache"
)

const (
	OneGig          = 1 << 20
	TwoGigs         = OneGig * 2
	MaxDomainsInMap = 32
	TickEvery       = 10 * time.Second
	Parallelism     = 2
	RandomDelay     = 15 * time.Second
	APIRetryMax     = 3
	//
	ReadTimeout  = 30 * time.Second
	WriteTimeout = 30 * time.Second
	IdleTimeout  = 60 * time.Second
	//
	GetDomainsRetry = 60 * time.Second
)

var (
	FreyaKey = os.Getenv("FREYA")                      // nolint:gochecknoglobals
	APIBase  = "https://api.domainsproject.org/api/vo" // nolint:gochecknoglobals
)

type JSONResponse struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

type DomainsResponse struct {
	Domains []string `json:"domains"`
}

func RunCrawl(target, serverAddr string, debugMode bool) {
	ctx := context.Background()

	args := []string{
		"-url",
		target,
		"-server",
		serverAddr,
	}

	if debugMode {
		args = append(args, "-debug")
	}

	cmd := exec.CommandContext(ctx, os.Args[:1][0], args...) // nolint:gosec
	sout, _ := cmd.StdoutPipe()
	serr, _ := cmd.StderrPipe()
	err := cmd.Start()
	//
	if err != nil {
		log.Error(err)

		return
	}

	pipes := io.MultiReader(sout, serr)
	scanner := bufio.NewScanner(pipes)
	//
	for scanner.Scan() {
		ucl := strings.ToUpper(scanner.Text())
		log.Println(ucl)
	}

	err = cmd.Wait()

	if err != nil {
		log.Errorf("Could not start crawler: %+v\n", err)
	}
}

func RunWithAPI(client *Client, address string, debugMode bool, srvr *S) {
	for {
		domains, err := client.GetDomains()
		if err != nil {
			time.Sleep(GetDomainsRetry)

			continue
		}
		// Starting crawlers is expensive, do HEAD check first
		checkedMap := HeadCheckDomains(domains, srvr.userAgent)
		//

		for domain, ok := range checkedMap {
			if !ok {
				continue
			}

			RunCrawl(domain, address, debugMode)
		}
		// time to empty out cache
		for {
			domain := srvr.Pop()
			if len(domain) == 0 {
				break
			}

			RunCrawl(domain, address, debugMode)
		}
	}
}

func main() { // nolint:funlen
	debugMode := flag.Bool("debug", false, "Enable colly/crawler debugging")
	targetURL := flag.String("url", "", "URL/Domain to crawl")
	serverAddr := flag.String("server", "", "Local supervisor address")
	domainsFile := flag.String("file", "", "Domains file, one domain per line")
	yacy := flag.Bool("yacy", false, "Get hosts from Yacy.net FreeWorld network and crawl them")
	yacyAddr := flag.String("yacy-addr", "http://127.0.0.1:8090", "Yacy.net address, defaults to localhost")
	single := flag.Bool("single", false, "Start with single url. For debugging.")
	flag.Parse()

	logger := log.New()

	if *debugMode {
		logger.SetLevel(log.DebugLevel)
	}

	client := &Client{
		Key:     FreyaKey,
		Logger:  logger,
		APIBase: APIBase,
	}

	ua, err := client.GetUA()
	if err != nil {
		panic(err)
	}

	s := &S{cache: memcache.New(logger), userAgent: ua}

	r := mux.NewRouter()
	r.HandleFunc("/upload", s.UploadDomains).Methods(http.MethodPost)
	r.HandleFunc("/ua", s.UA).Methods(http.MethodGet)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	//
	Address := listener.Addr().String()
	_ = listener.Close()

	server := &http.Server{
		Addr:         Address,
		Handler:      r,
		ReadTimeout:  ReadTimeout,
		WriteTimeout: WriteTimeout,
		IdleTimeout:  IdleTimeout,
	}

	go func() {
		log.Println("Starting internal listener at ", Address)

		if err := server.ListenAndServe(); err != nil {
			panic(err)
		}
	}()

	if *yacy {
		log.Println("Starting Yacy.net mode")
		CrawlYacyHosts(*yacyAddr, Address, *debugMode, s)

		return
	}

	if *single {
		log.Println("Starting single URL mode")
		RunCrawl(*targetURL, Address, *debugMode)

		return
	}

	if len(*domainsFile) == 0 {
		log.Println("Starting normal mode")
		RunWithAPI(client, Address, *debugMode, s)

		return
	}

	if len(*targetURL) != 0 {
		log.Println("Starting crawl of ", *targetURL)
		CrawlURL(client, *targetURL, *debugMode, *serverAddr)

		return
	}
	//
	// FALLBACK TO FILE MODE
	//
	log.Println("Starting file mode")

	f, err := os.Open(*domainsFile)
	if err != nil {
		panic(err)
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		RunCrawl(scanner.Text(), Address, *debugMode)

		// time to empty out cache
		for {
			domain := s.Pop()
			if len(domain) == 0 {
				break
			}

			RunCrawl(domain, Address, *debugMode)
		}
	}
}
