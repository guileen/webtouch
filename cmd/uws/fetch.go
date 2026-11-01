package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const fetchLimit = 4 << 20

func fetchCLI(args []string, out io.Writer) error {
	result := map[string]any{"contract": "webtouch.cli.fetch.v1", "backend": "http"}
	fail := func(code, message string) error {
		result["ok"] = false
		result["error"] = map[string]string{"code": code, "message": message}
		_ = json.NewEncoder(out).Encode(result)
		return errors.New(code)
	}
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	address := fs.String("url", "", "")
	format := fs.String("format", "markdown", "")
	timeout := fs.Int("timeout", 25000, "")
	if fs.Parse(args) != nil || fs.NArg() != 0 || !validFetchURL(*address) || *timeout < 1 || *timeout > 120000 {
		return fail("invalid_argument", "invalid fetch arguments")
	}
	if *format != "markdown" {
		return fail("unsupported_format", "only markdown is supported")
	}
	client := &http.Client{Timeout: time.Duration(*timeout) * time.Millisecond, Transport: &http.Transport{}, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !validFetchURL(req.URL.String()) {
			return errors.New("invalid redirect")
		}
		return nil
	}}
	defer client.CloseIdleConnections()
	resp, err := client.Get(*address)
	if err != nil {
		return fail("fetch_failed", "HTTP request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fail("http_status", fmt.Sprintf("HTTP status %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchLimit+1))
	if err != nil {
		return fail("fetch_failed", "HTTP response read failed")
	}
	if len(body) > fetchLimit {
		return fail("output_too_large", "response exceeds 4 MiB")
	}
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if media == "" {
		media, _, _ = mime.ParseMediaType(http.DetectContentType(body))
	}
	var content string
	switch media {
	case "text/plain":
		content = string(body)
	case "text/html", "application/xhtml+xml":
		root, parseErr := html.Parse(strings.NewReader(string(body)))
		if parseErr != nil {
			return fail("invalid_html", "HTML parse failed")
		}
		content = fetchMarkdown(root, resp.Request.URL)
	default:
		return fail("unsupported_content_type", "response is not HTML or plain text")
	}
	result["ok"] = true
	result["format"] = "markdown"
	result["content"] = content
	result["status"] = resp.StatusCode
	result["url"] = resp.Request.URL.String()
	return json.NewEncoder(out).Encode(result)
}

func validFetchURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u != nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && !strings.Contains(strings.ToLower(raw), "@vault:")
}

func fetchMarkdown(root *html.Node, base *url.URL) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			return
		}
		if n.Type != html.ElementNode && n.Type != html.DocumentNode {
			return
		}
		tag := n.Data
		switch tag {
		case "head", "script", "style", "noscript", "svg":
			return
		}
		switch tag {
		case "h1", "h2", "h3", "h4", "h5", "h6":
			b.WriteString("\n\n" + strings.Repeat("#", int(tag[1]-'0')) + " ")
		case "p", "div", "section", "article", "br", "ul", "ol":
			b.WriteString("\n\n")
		case "li":
			b.WriteString("\n- ")
		}
		var href string
		if tag == "a" {
			for _, a := range n.Attr {
				if a.Key == "href" {
					u, err := url.Parse(a.Val)
					if err == nil {
						resolved := base.ResolveReference(u)
						if validFetchURL(resolved.String()) {
							href = resolved.String()
						}
					}
					break
				}
			}
			if href != "" {
				b.WriteByte('[')
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if href != "" {
			b.WriteString("](" + href + ")")
		}
	}
	walk(root)
	lines := strings.Split(b.String(), "\n")
	var clean []string
	blank := false
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if !blank && len(clean) > 0 {
				clean = append(clean, "")
				blank = true
			}
			continue
		}
		clean = append(clean, line)
		blank = false
	}
	return strings.TrimSpace(strings.Join(clean, "\n"))
}
