// auditload drives the REAL audit sender (execute.Chain + execute.HTTPSink, what `--audit-file` + `--audit-url` use) against a
// collector, to measure throughput and what is lost. Lab use only (hack/lab-audit-load.sh).
//
//	auditload --url https://127.0.0.1:8443/ --ca-file ca.crt --token-file tok --events 2000 --rate 0 --file out.jsonl
//
// --rate 0 records every event at once (a burst); --rate N records N events per second. It prints one JSON line: events recorded,
// the seconds until the queue drained, events per second, and how many the sender reported as not delivered.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/execute"
)

func main() {
	url := flag.String("url", "", "collector URL.")
	caFile := flag.String("ca-file", "", "PEM CA the collector's certificate chains to.")
	tokenFile := flag.String("token-file", "", "bearer token file.")
	n := flag.Int("events", 1000, "events to record.")
	rate := flag.Float64("rate", 0, "events per second; 0 records them all at once.")
	file := flag.String("file", "", "chained local audit file (required: the copy the collector's dump is compared with).")
	label := flag.String("label", "run", "label for the result line.")
	flag.Parse()
	if *url == "" || *file == "" {
		log.Fatal("--url and --file are required")
	}
	tok := ""
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			log.Fatal(err)
		}
		tok = strings.TrimSpace(string(b))
	}
	hc, err := execute.AuditHTTPClient(*caFile)
	if err != nil {
		log.Fatal(err)
	}
	f, err := os.OpenFile(*file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	prev, err := execute.LastHash(*file)
	if err != nil {
		log.Fatal(err)
	}
	hs := execute.NewHTTPSink(*url, tok, hc)
	ch := execute.NewChain(f, prev)
	ch.Next = hs

	start := time.Now()
	var gap time.Duration
	if *rate > 0 {
		gap = time.Duration(float64(time.Second) / *rate)
	}
	for i := 0; i < *n; i++ {
		ch.Record(execute.Event{Time: time.Now().UTC(), Type: "load", Actor: "auditload", Target: *label,
			Details: map[string]any{"seq": i, "of": *n}})
		if gap > 0 {
			time.Sleep(gap)
		}
	}
	recorded := time.Since(start)
	cerr := hs.Close()
	took := time.Since(start)
	dropped := 0
	if cerr != nil {
		fmt.Sscanf(cerr.Error(), "%d", &dropped)
	}
	out := map[string]any{"label": *label, "events": *n, "rate": *rate, "recordSeconds": recorded.Seconds(), "drainSeconds": took.Seconds(),
		"deliveredPerSecond": float64(*n-dropped) / took.Seconds(), "notDelivered": dropped}
	b, _ := json.Marshal(out)
	fmt.Println(string(b))
}
