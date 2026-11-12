// audit-collector is a reference append-only collector for `--audit-url` (the rightsize CLI and the operator's applier).
//
//	serve         HTTPS (or plain HTTP on localhost only), bearer token. Each POSTed JSON audit event becomes ONE object in an S3 bucket
//	              that has Object Lock enabled, written with a retention (COMPLIANCE by default): until it expires, no credential,
//	              the collector's own included, can overwrite or delete that object version. 2xx is answered only after the object
//	              is stored, so the sender's retry covers a store outage. The bucket must already have Object Lock (or is created with
//	              it); a bucket without it is refused, never written to silently.
//	dump          writes the stored events in arrival order as JSONL (for `rightsize audit-verify`).
//	probe-delete  tries to delete the first stored event's version and reports whether the store refused (it must).
//
// S3 credentials come from S3_ACCESS_KEY / S3_SECRET_KEY. It works with any S3 API that implements Object Lock (AWS S3, MinIO, ...).
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type store struct {
	c      *minio.Client
	bucket string
}

func newStore(endpoint, bucket string, secure bool) (*store, error) {
	ak, sk := os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY")
	if ak == "" || sk == "" {
		return nil, errors.New("S3_ACCESS_KEY and S3_SECRET_KEY must be set")
	}
	c, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: secure})
	if err != nil {
		return nil, err
	}
	return &store{c: c, bucket: bucket}, nil
}

// ensureLocked creates the bucket with Object Lock if it does not exist, and refuses one that exists without it.
func (s *store) ensureLocked(ctx context.Context, create bool) error {
	ok, err := s.c.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if !ok {
		if !create {
			return fmt.Errorf("bucket %q does not exist", s.bucket)
		}
		if err := s.c.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{ObjectLocking: true}); err != nil {
			return err
		}
	}
	enabled, _, _, _, err := s.c.GetObjectLockConfig(ctx, s.bucket)
	if err != nil || enabled != "Enabled" {
		return fmt.Errorf("bucket %q has no Object Lock (%v): refusing to write an audit trail that can be deleted", s.bucket, err)
	}
	return nil
}

type server struct {
	st     *store
	token  string
	mode   minio.RetentionMode
	retain time.Duration
	mu     sync.Mutex // one write at a time: the keys keep the arrival order
	seq    int
}

func (sv *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if sv.token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+sv.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 {
		http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	body = bytes.TrimSpace(body)
	if !json.Valid(body) || len(body) == 0 || body[0] != '{' {
		http.Error(w, "one JSON object per request", http.StatusBadRequest)
		return
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	sv.seq++
	sum := sha256.Sum256(body)
	key := fmt.Sprintf("events/%s-%08d-%s.json", time.Now().UTC().Format("20060102T150405.000000000Z"), sv.seq, hex.EncodeToString(sum[:8]))
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	_, err = sv.st.c.PutObject(ctx, sv.st.bucket, key, bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{
		ContentType: "application/json", Mode: sv.mode, RetainUntilDate: time.Now().Add(sv.retain).UTC(), SendContentMd5: true})
	if err != nil {
		log.Printf("store %s: %v", key, err)
		http.Error(w, "not stored", http.StatusServiceUnavailable) // the sender retries
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *store) keys(ctx context.Context) ([]minio.ObjectInfo, error) {
	var out []minio.ObjectInfo
	for o := range s.c.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: "events/", Recursive: true}) {
		if o.Err != nil {
			return nil, o.Err
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: audit-collector serve|dump|probe-delete [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	endpoint := fs.String("s3-endpoint", "", "S3 endpoint host:port (required).")
	bucket := fs.String("bucket", "kubecost-audit", "bucket with Object Lock.")
	insecureS3 := fs.Bool("s3-plain-http", false, "talk to the S3 endpoint over plain HTTP (a lab MinIO only).")
	listen := fs.String("listen", ":8443", "serve: listen address.")
	cert := fs.String("tls-cert", "", "serve: TLS certificate (PEM).")
	key := fs.String("tls-key", "", "serve: TLS key (PEM).")
	tokenFile := fs.String("token-file", "", "serve: file with the bearer token senders must present.")
	mode := fs.String("mode", "COMPLIANCE", "serve: Object Lock mode, COMPLIANCE or GOVERNANCE.")
	retain := fs.Duration("retain", 24*time.Hour, "serve: how long each event is locked.")
	create := fs.Bool("create-bucket", false, "serve: create the bucket (with Object Lock) if missing.")
	_ = fs.Parse(os.Args[2:])
	if *endpoint == "" {
		log.Fatal("--s3-endpoint is required")
	}
	st, err := newStore(*endpoint, *bucket, !*insecureS3)
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	switch os.Args[1] {
	case "serve":
		m := minio.RetentionMode(strings.ToUpper(*mode))
		if !m.IsValid() {
			log.Fatalf("--mode must be COMPLIANCE or GOVERNANCE, not %q", *mode)
		}
		if err := st.ensureLocked(ctx, *create); err != nil {
			log.Fatal(err)
		}
		tok := ""
		if *tokenFile != "" {
			b, err := os.ReadFile(*tokenFile)
			if err != nil {
				log.Fatal(err)
			}
			tok = strings.TrimSpace(string(b))
		}
		sv := &server{st: st, token: tok, mode: m, retain: *retain}
		hs := &http.Server{Addr: *listen, Handler: sv, ReadHeaderTimeout: 10 * time.Second}
		log.Printf("serving on %s, bucket %s, %s retention %s", *listen, *bucket, m, *retain)
		if *cert != "" {
			log.Fatal(hs.ListenAndServeTLS(*cert, *key))
		}
		if !strings.HasPrefix(*listen, "127.0.0.1:") && !strings.HasPrefix(*listen, "localhost:") {
			log.Fatal("without --tls-cert the collector listens on localhost only")
		}
		log.Fatal(hs.ListenAndServe())
	case "dump":
		objs, err := st.keys(ctx)
		if err != nil {
			log.Fatal(err)
		}
		for _, o := range objs {
			r, err := st.c.GetObject(ctx, *bucket, o.Key, minio.GetObjectOptions{})
			if err != nil {
				log.Fatal(err)
			}
			b, err := io.ReadAll(r)
			r.Close()
			if err != nil {
				log.Fatal(err)
			}
			fmt.Println(string(bytes.TrimSpace(b)))
		}
	case "probe-delete":
		objs, err := st.keys(ctx)
		if err != nil || len(objs) == 0 {
			log.Fatalf("nothing stored (%v)", err)
		}
		o := objs[0]
		info, err := st.c.StatObject(ctx, *bucket, o.Key, minio.StatObjectOptions{})
		if err != nil {
			log.Fatal(err)
		}
		mode, until := "", ""
		if info.Metadata != nil {
			mode, until = info.Metadata.Get("X-Amz-Object-Lock-Mode"), info.Metadata.Get("X-Amz-Object-Lock-Retain-Until-Date")
		}
		err = st.c.RemoveObject(ctx, *bucket, o.Key, minio.RemoveObjectOptions{VersionID: info.VersionID, GovernanceBypass: true})
		res := map[string]any{"key": o.Key, "versionId": info.VersionID, "lockMode": mode, "retainUntil": until, "deleteRefused": err != nil}
		if err != nil {
			res["error"] = err.Error()
		}
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		if err == nil {
			os.Exit(1)
		}
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}
