package main

import (
	"bytes"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudfront"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	ghtml "github.com/yuin/goldmark/renderer/html"
)

var (
	deploy bool
	dir    string
	bucket string
	distid string
	sess   *session.Session
)

func check(err error) {
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func main() {
	flag.StringVar(&bucket, "bucket", "indexsupply.com", "s3 bucket")
	flag.BoolVar(&deploy, "deploy", false, "deploy to s3")
	flag.StringVar(&dir, "dir", "src", "root dir")
	flag.StringVar(&distid, "distid", "E100U1X0OYQONF", "cloudfront distribution")
	flag.Parse()

	sess = session.Must(session.NewSession())

	if deploy {
		check(filepath.Walk(dir, upload))
		check(invalidate())
		return
	}
	http.HandleFunc("/", serve)
	http.ListenAndServe(":8080", nil)
}

func isdir(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.IsDir()
}

func html(p string) []byte {
	if isdir(p) {
		p = path.Join(p, "index.html")
	}
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	b, err := ioutil.ReadAll(f)
	check(err)
	return b
}

func serve(w http.ResponseWriter, r *http.Request) {
	p := path.Join(dir, r.URL.Path)

	if res := html(p); len(res) > 0 {
		fmt.Fprint(w, string(res))
		return
	}

	if isdir(p) {
		p = path.Join(p, "index.md")
	} else {
		p += ".md"
	}
	res, err := render(p)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	fmt.Fprint(w, string(res))
}

func render(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("opening file %s: %w", p, err)
	}
	defer f.Close()

	src, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", f.Name(), err)
	}
	var buf bytes.Buffer
	md := goldmark.New(
		goldmark.WithRendererOptions(
			ghtml.WithHardWraps(),
			ghtml.WithUnsafe(),
			ghtml.WithXHTML(),
		),
		goldmark.WithExtensions(
			extension.Table,
			extension.Typographer,
			extension.GFM,
		),
		goldmark.WithParserOptions(
			parser.WithAttribute(),
			parser.WithAutoHeadingID(),
		),
	)
	err = md.Convert(src, &buf)
	if err != nil {
		return nil, fmt.Errorf("md converting %s: %w", f.Name(), err)
	}
	res, err := layout(f.Name(), buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("adding layout %s: %w", f.Name(), err)
	}
	return updateMetadata(res)
}

func updateMetadata(src []byte) ([]byte, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("loading goquery doc: %w", err)
	}
	mdt := doc.Find(`body title`).First()
	mdt.Remove()
	doc.Find("title").SetText(mdt.Text())

	mds := doc.Find(`body style`)
	mds.Remove()
	mdsHtml, err := goquery.OuterHtml(mds)
	if err != nil {
		return nil, fmt.Errorf("rendering style html: %w", err)
	}
	doc.Find("head").AppendHtml(mdsHtml)

	r, err := doc.Find("html").Html()
	return []byte(r), err
}

func layout(p string, src []byte) ([]byte, error) {
	var (
		dirs = strings.Split(path.Dir(p), "/")
		l    []byte
	)
	for i := len(dirs); i >= 0 && len(l) == 0; i-- {
		dir := path.Join(dirs[:i]...)
		l, _ = os.ReadFile(path.Join(dir, "_layout.html"))
		if len(l) > 0 {
			break
		}
	}
	if len(l) == 0 {
		return nil, fmt.Errorf("unable to find a _layout.html file")
	}
	return bytes.Replace(l, []byte("{{Body}}"), src, 1), nil
}

func upload(p string, fi os.FileInfo, err error) error {
	switch {
	case err != nil:
		return err
	case strings.HasPrefix(fi.Name(), "_"):
		return nil
	case fi.Name() == "index.md":
		return nil
	case fi.IsDir():
		b, err := render(path.Join(p, "index.md"))
		if err != nil {
			fmt.Printf("skipping %s: %s\n", p, err)
			return nil
		}
		var key string
		key = fmt.Sprintf("%s/index.html", p)
		key = strings.TrimPrefix(key, dir)
		return putFile(b, bucket, key)
	case filepath.Ext(p) == ".md":
		b, err := render(p)
		if err != nil {
			fmt.Printf("skipping %s: %s\n", p, err)
			return nil
		}
		var key string
		key = strings.TrimPrefix(p, dir)
		key = strings.TrimSuffix(key, filepath.Ext(key))
		return putFile(b, bucket, key)
	case filepath.Ext(p) == ".html":
		f, err := os.Open(p)
		if err != nil {
			fmt.Printf("skipping %s: %s\n", f.Name(), err)
			return nil
		}
		defer f.Close()
		b, err := io.ReadAll(f)
		if err != nil {
			return fmt.Errorf("reading %s: %w", f.Name(), err)
		}
		key := strings.TrimPrefix(p, dir)
		return putFile(b, bucket, key)
	default:
		return nil
	}
}

func putFile(b []byte, bucket, key string) error {
	_, err := s3manager.NewUploader(sess).Upload(&s3manager.UploadInput{
		Bucket:      &bucket,
		Key:         &key,
		Body:        bytes.NewReader(b),
		ContentType: aws.String(http.DetectContentType(b)),
	})
	return err
}

func invalidate() error {
	var ref [1 << 5]byte
	rand.Read(ref[:])
	_, err := cloudfront.New(sess).CreateInvalidation(&cloudfront.CreateInvalidationInput{
		DistributionId: &distid,
		InvalidationBatch: &cloudfront.InvalidationBatch{
			CallerReference: aws.String(string(ref[:])),
			Paths: &cloudfront.Paths{
				Quantity: aws.Int64(1),
				Items:    []*string{aws.String("/*")},
			},
		},
	})
	return err
}
