package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

const (
	readmeFileName   = "README.md"
	indexURLTemplate = "http://tv.cctv.com/lm/xwlb/day/%s.shtml"
	indexDateLayout  = "20060102"
	stampLayout      = "2006-01-02 15:04"
	htmlBodyTag      = "body"
	insertMarker     = "<!-- INSERT -->"
	maxConcurrent    = 64
	httpTimeout      = 5 * time.Second
	maxIdleConns     = 256
	maxIdlePerHost   = 128
	keepAliveTimeout = 30 * time.Second
)

var (
	rootDir        string
	readmePath     string
	utc8           = time.FixedZone("UTC+8", 8*60*60)
	titleCleanup   = []string{"[视频]", "[Video]"}
	shtmlPattern   = ".shtml"
	cctvTagPattern = regexp.MustCompile(`<strong>央视网消息</strong>（新闻联播）：`)
	readmeDateRe   = regexp.MustCompile(`-\s+\[(\d{8})\]\(\./\d{4}/\d{8}\.md\)`)
	baseHeaders    = map[string]string{
		"accept":           "text/html,*/*;q=0.01",
		"accept-language":  "en-US,en;q=0.9",
		"cache-control":    "no-cache",
		"sec-ch-ua":        `"Edge";v="107","Chromium";v="107"`,
		"sec-ch-ua-mobile": "?0",
		"sec-fetch-dest":   "empty",
		"sec-fetch-mode":   "cors",
		"x-requested-with": "XMLHttpRequest",
		"Referrer-Policy":  "strict-origin-when-cross-origin",
	}
)

type itemEntry struct {
	Title   string
	Payload []byte
	Link    string
}

func init() {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		cwd = "."
	}
	rootDir = mustAbs(cwd)
	readmePath = filepath.Join(rootDir, readmeFileName)
}

func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil || abs == "" {
		return path
	}
	return abs
}

func nowUTC8() time.Time {
	return time.Now().In(utc8)
}

func datecode(now time.Time) string {
	if now.IsZero() {
		now = nowUTC8()
	}
	return now.Format(indexDateLayout)
}

func timetag(now time.Time) string {
	if now.IsZero() {
		now = nowUTC8()
	}
	return now.Format(stampLayout)
}

func newHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdlePerHost,
		IdleConnTimeout:       keepAliveTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Timeout: httpTimeout, Transport: transport}
}

func readResponseBody(resp *http.Response) ([]byte, error) {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func pullBytes(ctx context.Context, client *http.Client, target string) ([]byte, error) {
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("invalid target url: %q", target)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("request build: %w", err)
	}
	for k, v := range baseHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Referer", target)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return readResponseBody(resp)
}

func pullIndex(ctx context.Context, client *http.Client, day string) ([]string, error) {
	target := fmt.Sprintf(indexURLTemplate, day)
	payload, err := pullBytes(ctx, client, target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pull index:", err)
		return nil, nil
	}

	root, err := html.Parse(bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}

	return collectLinks(root, target)
}

func pullItem(ctx context.Context, client *http.Client, link string) itemEntry {
	payload, err := pullBytes(ctx, client, link)
	if err != nil {
		return itemEntry{Link: link}
	}
	root, err := html.Parse(bytes.NewReader(payload))
	if err != nil {
		return itemEntry{Link: link}
	}
	titleNode, bodyNode := findTitleAndBody(root)
	title := strings.TrimSpace(textContent(titleNode))
	for _, pattern := range titleCleanup {
		title = strings.ReplaceAll(title, pattern, "")
	}
	title = strings.TrimSpace(title)

	bodyHTML := ""
	if bodyNode != nil {
		var buf bytes.Buffer
		if err := html.Render(&buf, bodyNode); err == nil {
			bodyHTML = buf.String()
		}
	}

	return itemEntry{Title: title, Payload: []byte(bodyHTML), Link: link}
}

func pullBatch(ctx context.Context, client *http.Client, links []string) []itemEntry {
	total := len(links)
	if total == 0 {
		return nil
	}
	limit := min(total, max(maxConcurrent, 1))

	results := make([]itemEntry, total)
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, link := range links {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, link string) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = pullItem(ctx, client, link)
		}(i, link)
	}
	wg.Wait()
	return results
}

func renderMarkdown(items []itemEntry) string {
	stamp := timetag(time.Time{})
	var b strings.Builder
	b.WriteString("- 时间：")
	b.WriteString(stamp)
	b.WriteString("\n")

	for _, item := range items {
		title := strings.TrimSpace(item.Title)
		if title == "" || strings.Contains(title, "新闻联播") {
			continue
		}
		body := ""
		if len(item.Payload) > 0 {
			cleaned := cctvTagPattern.ReplaceAll(item.Payload, nil)
			body = strings.TrimSpace(htmlToMarkdown(cleaned))
		}
		b.WriteString("\n## ")
		b.WriteString(title)
		b.WriteString("\n")
		if body != "" {
			b.WriteString(body)
			b.WriteString("\n\n")
		}
		b.WriteString("- [链接](")
		b.WriteString(item.Link)
		b.WriteString(")\n")
	}
	return b.String()
}

func syncCatalog(day string, docPath string) error {
	readmePayload, err := os.ReadFile(readmePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	relative, err := filepath.Rel(rootDir, docPath)
	if err != nil {
		return err
	}
	record := fmt.Sprintf("- [%s](./%s)", day, filepath.ToSlash(relative))
	readme := string(readmePayload)
	if strings.Contains(readme, record) || !strings.Contains(readme, insertMarker) {
		return nil
	}
	updated := strings.Replace(readme, insertMarker, insertMarker+"\n"+record, 1)
	tmpPath := readmePath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(updated), 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, readmePath)
}

func computeDaysToFetch(today string, seen map[string]struct{}) []string {
	if len(seen) == 0 {
		return []string{today}
	}
	var dates []string
	for day := range seen {
		if len(day) == 8 {
			dates = append(dates, day)
		}
	}
	if len(dates) == 0 {
		return []string{today}
	}
	sort.Strings(dates)
	last := dates[len(dates)-1]
	lastDate, err := time.ParseInLocation(indexDateLayout, last, utc8)
	if err != nil {
		return []string{today}
	}
	todayDate, err := time.ParseInLocation(indexDateLayout, today, utc8)
	if err != nil {
		return []string{today}
	}
	if lastDate.After(todayDate) {
		return []string{today}
	}
	span := int(todayDate.Sub(lastDate).Hours() / 24)
	if span <= 0 {
		return []string{today}
	}
	days := make([]string, 0, span+1)
	for i := 0; i <= span; i++ {
		d := lastDate.AddDate(0, 0, i).Format(indexDateLayout)
		if _, ok := seen[d]; !ok {
			days = append(days, d)
		}
	}
	if len(days) == 0 {
		return []string{today}
	}
	if !slices.Contains(days, today) {
		days = append(days, today)
	}
	return days
}

func processDay(ctx context.Context, client *http.Client, day string) error {
	if len(day) < 8 {
		return fmt.Errorf("invalid date: %s", day)
	}
	yearDir := filepath.Join(rootDir, day[:4])
	docPath := filepath.Join(yearDir, day+".md")
	if err := os.MkdirAll(yearDir, 0o755); err != nil {
		return err
	}
	links, err := pullIndex(ctx, client, day)
	if err != nil {
		return err
	}
	if len(links) == 0 {
		return nil
	}
	items := pullBatch(ctx, client, links)
	if len(items) == 0 {
		return nil
	}
	content := formatMarkdown([]byte(renderMarkdown(items)))
	tmpPath := docPath + ".tmp"
	if err := os.WriteFile(tmpPath, content, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, docPath); err != nil {
		return err
	}
	return syncCatalog(day, docPath)
}

func main() {
	var dateFlag string
	flag.StringVar(&dateFlag, "date", "", "date in YYYYMMDD")
	flag.Parse()

	today := datecode(time.Time{})
	if dateFlag != "" {
		if _, err := time.ParseInLocation(indexDateLayout, dateFlag, utc8); err == nil {
			today = dateFlag
		} else {
			fmt.Fprintln(os.Stderr, "Invalid date:", dateFlag)
			os.Exit(2)
		}
	}

	seen, err := seenDatesFromReadme(readmePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	client := newHTTPClient()
	ctx := context.Background()
	for _, day := range computeDaysToFetch(today, seen) {
		if err := processDay(ctx, client, day); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
	}
}

func seenDatesFromReadme(path string) (map[string]struct{}, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]struct{}{}, nil
		}
		return nil, err
	}
	matches := readmeDateRe.FindAllStringSubmatch(string(payload), -1)
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		if len(match) > 1 {
			seen[match[1]] = struct{}{}
		}
	}
	return seen, nil
}

func getAttr(node *html.Node, name string) string {
	for _, attr := range node.Attr {
		if attr.Key == name {
			return attr.Val
		}
	}
	return ""
}

func textContent(node *html.Node) string {
	if node == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(node)
	return strings.TrimSpace(strings.Join(strings.Fields(b.String()), " "))
}

func htmlToMarkdown(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	root, err := html.Parse(bytes.NewReader(payload))
	if err != nil {
		return ""
	}
	body := findFirst(root, func(n *html.Node) bool { return n.Type == html.ElementNode && n.Data == htmlBodyTag })
	if body == nil {
		body = root
	}
	var out strings.Builder
	renderNode(&out, body, 0, false, false)
	return strings.TrimSpace(out.String())
}

func collectLinks(root *html.Node, baseURL string) ([]string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	links := make([]string, 0, 32)
	walk(root, func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "a" {
			href := strings.TrimSpace(getAttr(n, "href"))
			if href == "" || !strings.Contains(href, shtmlPattern) {
				return false
			}
			ref, err := url.Parse(href)
			if err != nil {
				return false
			}
			resolved := base.ResolveReference(ref)
			if resolved == nil {
				return false
			}
			if resolved.Scheme != "http" && resolved.Scheme != "https" {
				return false
			}
			if resolved.Host == "" {
				return false
			}
			absolute := resolved.String()
			if _, exists := seen[absolute]; exists {
				return false
			}
			seen[absolute] = struct{}{}
			links = append(links, absolute)
		}
		return false
	})
	return links, nil
}

func findTitleAndBody(root *html.Node) (*html.Node, *html.Node) {
	var titleNode *html.Node
	var bodyNode *html.Node
	walk(root, func(n *html.Node) bool {
		if n.Type != html.ElementNode {
			return false
		}
		if bodyNode == nil && n.Data == "div" {
			if id := getAttr(n, "id"); id == "content_area" {
				bodyNode = n
				if titleNode != nil {
					return true
				}
			}
		}
		if titleNode == nil && hasClass(n, "tit") {
			titleNode = n
			if bodyNode != nil {
				return true
			}
		}
		return false
	})
	return titleNode, bodyNode
}

func findFirst(root *html.Node, pred func(*html.Node) bool) *html.Node {
	var found *html.Node
	walk(root, func(n *html.Node) bool {
		if pred(n) {
			found = n
			return true
		}
		return false
	})
	return found
}

func walk(root *html.Node, visit func(*html.Node) bool) bool {
	if root == nil {
		return false
	}
	stack := []*html.Node{root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visit(n) {
			return true
		}
		for c := n.LastChild; c != nil; c = c.PrevSibling {
			stack = append(stack, c)
		}
	}
	return false
}

func hasClass(n *html.Node, className string) bool {
	if n == nil {
		return false
	}
	classAttr := getAttr(n, "class")
	if classAttr == "" {
		return false
	}
	return slices.Contains(strings.Fields(classAttr), className)
}

func renderNode(out *strings.Builder, node *html.Node, listDepth int, inPre bool, inCode bool) {
	for c := node.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			text := c.Data
			if !inPre {
				text = strings.Join(strings.Fields(text), " ")
			}
			out.WriteString(text)
			continue
		}
		if c.Type != html.ElementNode {
			renderNode(out, c, listDepth, inPre, inCode)
			continue
		}

		tag := strings.ToLower(c.Data)
		switch tag {
		case "br":
			out.WriteString("\n")
		case "p", "div":
			out.WriteString("\n")
			renderNode(out, c, listDepth, inPre, inCode)
			out.WriteString("\n")
		case "h1", "h2", "h3", "h4", "h5", "h6":
			level := int(tag[1] - '0')
			if level < 1 || level > 6 {
				level = 2
			}
			out.WriteString("\n")
			out.WriteString(strings.Repeat("#", level))
			out.WriteString(" ")
			renderNode(out, c, listDepth, inPre, inCode)
			out.WriteString("\n")
		case "ul", "ol":
			out.WriteString("\n")
			renderNode(out, c, listDepth+1, inPre, inCode)
			out.WriteString("\n")
		case "li":
			out.WriteString("\n")
			if listDepth > 1 {
				out.WriteString(strings.Repeat("  ", listDepth-1))
			}
			out.WriteString("- ")
			renderNode(out, c, listDepth, inPre, inCode)
		case "a":
			href := strings.TrimSpace(getAttr(c, "href"))
			text := textContent(c)
			if text == "" {
				text = href
			}
			if href == "" {
				out.WriteString(text)
			} else {
				out.WriteString("[")
				out.WriteString(text)
				out.WriteString("](")
				out.WriteString(href)
				out.WriteString(")")
			}
		case "strong", "b":
			out.WriteString("**")
			renderNode(out, c, listDepth, inPre, inCode)
			out.WriteString("**")
		case "pre":
			out.WriteString("\n```\n")
			renderNode(out, c, listDepth, true, true)
			out.WriteString("\n```\n")
		case "code":
			if inPre || inCode {
				renderNode(out, c, listDepth, inPre, true)
				break
			}
			out.WriteString("`")
			renderNode(out, c, listDepth, inPre, true)
			out.WriteString("`")
		default:
			renderNode(out, c, listDepth, inPre, inCode)
		}
	}
}

func formatMarkdown(input []byte) []byte {
	text := strings.ReplaceAll(string(input), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")

	out := make([]string, 0, len(lines)+8)
	inFence := false
	blankStreak := 0
	pendingHeading := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			out = append(out, line)
			blankStreak = 0
			pendingHeading = false
			continue
		}

		if !inFence {
			line = strings.TrimRight(line, " \t")
			if strings.TrimSpace(line) == "" {
				blankStreak++
				if blankStreak > 1 {
					continue
				}
				out = append(out, "")
				continue
			}
			blankStreak = 0

			if isHeadingLine(line) {
				pendingHeading = true
				out = append(out, line)
				continue
			}

			if pendingHeading {
				if len(out) > 0 && out[len(out)-1] != "" {
					out = append(out, "")
				}
				pendingHeading = false
			}
		}

		out = append(out, line)
	}

	result := strings.Join(out, "\n")
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return []byte(result)
}

func isHeadingLine(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, "#") {
		return false
	}
	i := 0
	for i < len(trimmed) && trimmed[i] == '#' {
		i++
	}
	if i == 0 || i > 6 {
		return false
	}
	if i < len(trimmed) && trimmed[i] != ' ' && trimmed[i] != '\t' {
		return false
	}
	return true
}
