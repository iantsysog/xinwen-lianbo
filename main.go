package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
)

const (
	readmeFileName   = "README.md"
	titleSelector    = ".video18847 .playingVideo .tit,.tit"
	bodySelector     = "#content_area"
	anchorSelector   = "a[href]"
	htmlBodySelector = "body"
	retries          = 3
	pauseBase        = 500 * time.Millisecond
	pauseMultiplier  = 1.5
	maxConcurrent    = 64
	httpTimeout      = 5 * time.Second
	maxIdleConns     = 256
	maxIdlePerHost   = 128
	keepAliveTimeout = 30 * time.Second
)

var (
	rootDir, _     = filepath.Abs(mustCwd())
	readmePath     = filepath.Join(rootDir, readmeFileName)
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
	titleSel  = mustSelector(titleSelector)
	bodySel   = mustSelector(bodySelector)
	anchorSel = mustSelector(anchorSelector)
	bodySelHT = mustSelector(htmlBodySelector)
)

type ctxKey struct{}

type itemEntry struct {
	Title   string
	Payload []byte
	Link    string
}

type stderrLogger struct {
	out io.Writer
	mu  sync.Mutex
}

func mustCwd() string {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		return "."
	}
	return cwd
}

func nowUTC8() time.Time {
	return time.Now().In(utc8)
}

func datecode(now time.Time) string {
	if now.IsZero() {
		now = nowUTC8()
	}
	return now.Format("20060102")
}

func timetag(now time.Time) string {
	if now.IsZero() {
		now = nowUTC8()
	}
	return now.Format("2006-01-02 15:04")
}

func newHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdlePerHost,
		IdleConnTimeout:       keepAliveTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Timeout:   httpTimeout,
		Transport: transport,
	}
}

func contextWithClient(ctx context.Context, client *http.Client) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, ctxKey{}, client)
}

func clientFromContext(ctx context.Context) *http.Client {
	if ctx == nil {
		return http.DefaultClient
	}
	if client, ok := ctx.Value(ctxKey{}).(*http.Client); ok && client != nil {
		return client
	}
	return http.DefaultClient
}

func readResponseBody(resp *http.Response) ([]byte, error) {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func sleepWithContext(ctx context.Context, delay time.Duration) bool {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func pullBytes(ctx context.Context, target string) ([]byte, error) {
	client := clientFromContext(ctx)
	delay := pauseBase
	var lastErr error

	for attempt := 0; attempt < retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
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
		if err == nil {
			body, readErr := readResponseBody(resp)
			_ = resp.Body.Close()
			if readErr == nil {
				return body, nil
			}
			err = readErr
		}
		lastErr = err

		if attempt < retries-1 {
			if !sleepWithContext(ctx, delay) {
				return nil, ctx.Err()
			}
			delay = time.Duration(float64(delay) * pauseMultiplier)
		}
	}

	return nil, fmt.Errorf("acquisition failure: %w", lastErr)
}

func pullIndex(ctx context.Context, day string) ([]string, error) {
	target := fmt.Sprintf("http://tv.cctv.com/lm/xwlb/day/%s.shtml", day)
	payload, err := pullBytes(ctx, target)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, nil
	}

	root, err := html.Parse(bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}

	base, err := url.Parse(target)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	links := make([]string, 0, 32)

	for _, node := range anchorSel.MatchAll(root) {
		href := strings.TrimSpace(getAttr(node, "href"))
		if href == "" || !strings.Contains(href, shtmlPattern) {
			continue
		}
		ref, err := url.Parse(href)
		if err != nil {
			continue
		}
		absolute := base.ResolveReference(ref).String()
		if _, exists := seen[absolute]; exists {
			continue
		}
		seen[absolute] = struct{}{}
		links = append(links, absolute)
	}

	return links, nil
}

func pullItem(ctx context.Context, link string) itemEntry {
	payload, err := pullBytes(ctx, link)
	if err != nil {
		return itemEntry{Link: link}
	}

	root, err := html.Parse(bytes.NewReader(payload))
	if err != nil {
		return itemEntry{Link: link}
	}

	title := strings.TrimSpace(textContent(matchFirst(root, titleSel)))
	for _, pattern := range titleCleanup {
		title = strings.ReplaceAll(title, pattern, "")
	}
	title = strings.TrimSpace(title)

	bodyNode := matchFirst(root, bodySel)
	bodyHTML := ""
	if bodyNode != nil {
		var buf bytes.Buffer
		if err := html.Render(&buf, bodyNode); err == nil {
			bodyHTML = buf.String()
		}
	}

	return itemEntry{
		Title:   title,
		Payload: []byte(bodyHTML),
		Link:    link,
	}
}

func pullBatch(ctx context.Context, links []string) []itemEntry {
	total := len(links)
	if total == 0 {
		return nil
	}

	limit := maxConcurrent
	if total < limit {
		limit = total
	}
	if limit < 1 {
		limit = 1
	}

	results := make([]itemEntry, total)
	jobs := make(chan struct {
		idx  int
		link string
	})

	var wg sync.WaitGroup
	wg.Add(limit)
	for i := 0; i < limit; i++ {
		go func() {
			defer wg.Done()
			for job := range jobs {
				if ctx.Err() != nil {
					return
				}
				results[job.idx] = pullItem(ctx, job.link)
			}
		}()
	}

	for i, link := range links {
		if ctx.Err() != nil {
			break
		}
		jobs <- struct {
			idx  int
			link string
		}{idx: i, link: link}
	}
	close(jobs)
	wg.Wait()

	return results
}

func renderMarkdown(items []itemEntry) string {
	stamp := timetag(time.Time{})
	var builder strings.Builder
	builder.WriteString("- 时间：")
	builder.WriteString(stamp)
	builder.WriteString("\n")

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

		builder.WriteString("\n## ")
		builder.WriteString(title)
		builder.WriteString("\n")
		if body != "" {
			builder.WriteString(body)
			builder.WriteString("\n\n")
		}
		builder.WriteString("- [链接](")
		builder.WriteString(item.Link)
		builder.WriteString(")\n")
	}

	return builder.String()
}

func syncCatalog(day string, docPath string) error {
	readmePayload, err := os.ReadFile(readmePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	marker := "<!-- INSERT -->"
	relative, err := filepath.Rel(rootDir, docPath)
	if err != nil {
		return err
	}
	record := fmt.Sprintf("- [%s](./%s)", day, filepath.ToSlash(relative))

	readme := string(readmePayload)
	if strings.Contains(readme, record) {
		return nil
	}
	if !strings.Contains(readme, marker) {
		return nil
	}

	updated := strings.Replace(readme, marker, marker+"\n"+record, 1)
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

	dates := make([]string, 0, len(seen))
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

	lastDate, err := time.ParseInLocation("20060102", last, utc8)
	if err != nil {
		return []string{today}
	}
	todayDate, err := time.ParseInLocation("20060102", today, utc8)
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
		d := lastDate.AddDate(0, 0, i).Format("20060102")
		if _, ok := seen[d]; !ok {
			days = append(days, d)
		}
	}
	if len(days) == 0 {
		return []string{today}
	}
	foundToday := false
	for _, d := range days {
		if d == today {
			foundToday = true
			break
		}
	}
	if !foundToday {
		days = append(days, today)
	}
	return days
}

func processDay(ctx context.Context, day string) error {
	if len(day) < 8 {
		return fmt.Errorf("invalid date: %s", day)
	}
	yearDir := filepath.Join(rootDir, day[:4])
	docPath := filepath.Join(yearDir, day+".md")

	if err := os.MkdirAll(yearDir, 0o755); err != nil {
		return err
	}

	links, err := pullIndex(ctx, day)
	if err != nil {
		return err
	}
	if len(links) == 0 {
		return nil
	}

	items := pullBatch(ctx, links)
	if len(items) == 0 {
		return nil
	}

	content := renderMarkdown(items)
	tmpPath := docPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(content), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, docPath); err != nil {
		return err
	}
	return syncCatalog(day, docPath)
}

func main() {
	var dateFlag string
	var fmtFlag bool
	flag.StringVar(&dateFlag, "date", "", "date in YYYYMMDD")
	flag.BoolVar(&fmtFlag, "fmt", false, "format all markdown files in current folder")
	flag.Parse()

	log := newStderrLogger(os.Stderr)

	if fmtFlag {
		if err := formatAllMarkdown(rootDir); err != nil {
			log.Error("Error: ", err)
			os.Exit(1)
		}
		return
	}

	today := datecode(time.Time{})
	if dateFlag != "" {
		if _, err := time.ParseInLocation("20060102", dateFlag, utc8); err == nil {
			today = dateFlag
		} else {
			log.Error("Invalid date: ", dateFlag)
			os.Exit(2)
		}
	}

	seen, err := seenDatesFromReadme(readmePath)
	if err != nil {
		log.Error("Error: ", err)
		os.Exit(1)
	}

	days := computeDaysToFetch(today, seen)
	if len(days) == 0 {
		return
	}

	client := newHTTPClient()
	ctx := contextWithClient(context.Background(), client)

	for _, day := range days {
		if err := processDay(ctx, day); err != nil {
			log.Error("Error: ", err)
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

func mustSelector(selector string) cascadia.Selector {
	return cascadia.MustCompile(selector)
}

func matchFirst(root *html.Node, selector cascadia.Selector) *html.Node {
	if root == nil {
		return nil
	}
	return selector.MatchFirst(root)
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
	body := matchFirst(root, bodySelHT)
	if body == nil {
		body = root
	}
	var out strings.Builder
	renderNode(&out, body, 0, false, false)
	return strings.TrimSpace(out.String())
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
			if inCode {
				renderNode(out, c, listDepth, inPre, inCode)
				break
			}
			out.WriteString("\n```\n")
			renderNode(out, c, listDepth, true, true)
			out.WriteString("\n```\n")
		default:
			renderNode(out, c, listDepth, inPre, inCode)
		}
	}
}

func formatAllMarkdown(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.ToLower(filepath.Ext(path)) != ".md" {
			return nil
		}
		original, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		formatted := formatMarkdown(original)
		if bytes.Equal(original, formatted) {
			return nil
		}
		tmpPath := path + ".tmp"
		if err := os.WriteFile(tmpPath, formatted, 0o644); err != nil {
			return err
		}
		return os.Rename(tmpPath, path)
	})
}

func newStderrLogger(out io.Writer) *stderrLogger {
	if out == nil {
		out = io.Discard
	}
	return &stderrLogger{out: out}
}

func (l *stderrLogger) write(args ...any) {
	if l == nil {
		return
	}
	msg := fmt.Sprint(args...)
	if msg == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = io.WriteString(l.out, msg)
	if !strings.HasSuffix(msg, "\n") {
		_, _ = io.WriteString(l.out, "\n")
	}
}

func (l *stderrLogger) Error(args ...any) {
	l.write(args...)
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
