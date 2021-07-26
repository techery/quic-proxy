package main

import (
	"flag"
	"fmt"
	"github.com/CAFxX/httpcompression"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	_ "net/http/pprof"

	"code.cloudfoundry.org/bytefmt"
	"github.com/elazarl/goproxy"
	log "github.com/liudanking/goutil/logutil"
	"github.com/techery/quic-proxy/common"
)


type Count struct {
	Id    string
	Count uint64
}

type CountReadCloser struct {
	Id string
	R  io.ReadCloser
	ch chan<- Count
	nr int64
}

func (c *CountReadCloser) Read(b []byte) (n int, err error) {
	n, err = c.R.Read(b)
	c.nr += int64(n)
	return
}

func (c CountReadCloser) Close() error {
	c.ch <- Count{c.Id, uint64(c.nr)}
	return c.R.Close()
}

func main() {
	log.Debug("SR Proxy started")

	var (
		listenAddr     string
		proxyUrl       string
		skipCertVerify bool
		auth           string
		verbose        bool
	)

	flag.StringVar(&listenAddr, "l", ":18080", "listenAddr")
	flag.StringVar(&proxyUrl, "proxy", "", "upstream proxy url")
	flag.BoolVar(&skipCertVerify, "k", false, "skip Cert Verify")
	flag.StringVar(&auth, "auth", "quic-proxy:Go!", "basic auth, format: username:password")
	flag.BoolVar(&verbose, "v", false, "verbose")
	flag.Parse()

	timer := make(chan bool)
	ch := make(chan Count, 10)
	go func() {
		for {
			time.Sleep(5 * time.Second)
			timer <- true
		}
	}()
	go func() {
		m := make(map[string]uint64)
		for {
			select {
			case c := <-ch:
				m[c.Id] = m[c.Id] + c.Count
			case <-timer:
				if len(m) > 0 {
					fmt.Printf("Statistics\n")
					for k, v := range m {
						fmt.Printf("%s -> %v\n", k, bytefmt.ByteSize(v))
					}
					m = make(map[string]uint64)
				}
			}
		}
	}()

	proxy := goproxy.NewProxyHttpServer()
	proxy.OnRequest().DoFunc(func(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		r.Header.Set("Accept-Encoding", "br")
		ctx.UserData = common.Stats{
			StartTime: time.Now(),
		}
		return r, nil
	})

	proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		if resp == nil {
			return nil
		}
		xRequestHeader := resp.Header["X-Requestaction"]
		if len(xRequestHeader) == 0 {
			return resp
		}
		action := resp.Header["X-Requestaction"][0]
		stats := ctx.UserData.(common.Stats)
		resp.Body = &CountReadCloser{action, resp.Body, ch, 0}
		log.Info("Call %v time: %v", action, time.Since(stats.StartTime))
		return resp
	})
	proxy.Verbose = verbose

	Url, err := url.Parse(proxyUrl)
	if err != nil {
		log.Error("proxyUrl:%s invalid", proxyUrl)
		return
	}
	if Url.Scheme == "https" {
		log.Error("quic-proxy only support http proxy")
		return
	}

	parts := strings.Split(auth, ":")
	if len(parts) != 2 {
		log.Error("auth param invalid")
		return
	}
	username, password := parts[0], parts[1]

	proxy.Tr.Proxy = func(req *http.Request) (*url.URL, error) {
		return url.Parse(proxyUrl)
	}

	dialer := common.NewQuicDialer(skipCertVerify)
	proxy.Tr.Dial = dialer.Dial

	proxy.ConnectDial = proxy.NewConnectDialToProxyWithHandler(proxyUrl,
		SetAuthForBasicConnectRequest(username, password))

	// set basic auth
	proxy.OnRequest().Do(SetAuthForBasicRequest(username, password))

	compress, _ := httpcompression.Adapter(httpcompression.BrotliCompressionLevel(11))

	log.Info("start serving %s", listenAddr)
	log.Error("%v", http.ListenAndServe(listenAddr, compress(proxy)))
}
