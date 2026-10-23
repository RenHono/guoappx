package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 每日大赛（mrds66）站源：Typecho 博客，视频以 HLS 发布。
//
// 上游接口（实测 2026-10-23）：
//   GET /archives.html            全部文章首页；翻页 /archives/page/{n}/
//   GET /category/{slug}/         分类文章；翻页 /category/{slug}/{n}/
//   GET /search/{keyword}/        站内搜索
//   GET /archives/{id}/           文章页（分集与播放地址都在这里）
//
// 文章页有两处可用结构，不要去正则硬啃正文：
//  1. JSON-LD WebPage.mainEntity 提供 headline / description / datePublished /
//     articleSection / keywords / author，video.duration 是 ISO-8601 时长。
//  2. <div class="dplayer" data-config='{...}'> 每个分集一个，data-config 内
//     video.url 是 H.264、video_h265.url 是 H.265，均为 AES-128 HLS。
//     m3u8 的 auth_key 带时效，播放地址必须每次现取，不能缓存。
//
// 列表页没有服务端渲染的缩略图，封面由脚本写成 background-image，
// HTML 里只留下 loadBannerDirect('<url>', '', ...#post-card-<id>...)，从那里取。

const (
	mrdsSiteBaseURL = "https://www.mrds66.com"
	mrdsSiteName    = "每日大赛"

	mrdsPageSize = 30
	mrdsMaxPage  = 2000
	mrdsMaxID    = 16
	mrdsMaxTitle = 512
)

var mrdsCategories = []nativeCategory{
	{ID: "mrds", Name: "每日大赛"},
	{ID: "ztds", Name: "主题大赛"},
	{ID: "rstt", Name: "热搜吃瓜"},
	{ID: "xazd", Name: "校园学生"},
	{ID: "blyp", Name: "必撸大赛"},
	{ID: "fctg", Name: "反差泄密"},
	{ID: "mhds", Name: "网红黑料"},
	{ID: "lqdp", Name: "猎奇重口"},
	{ID: "jdsj", Name: "AV看片"},
	{ID: "mxwh", Name: "明星大赛"},
	{ID: "smdh", Name: "动漫之家"},
	{ID: "dypd", Name: "影视国漫"},
	{ID: "mtds", Name: "cos写真"},
	{ID: "ysds", Name: "声控ASMR"},
	{ID: "czds", Name: "寸止挑战"},
	{ID: "hjds", Name: "混剪PMV"},
	{ID: "tgds", Name: "原创投稿"},
	{ID: "omjp", Name: "欧美精品"},
	{ID: "qwcs", Name: "全网参赛"},
	{ID: "aijc", Name: "AI剧场"},
}

var (
	reMrdsArchiveLink = regexp.MustCompile(`(?is)<a\b[^>]*href="/archives/(\d+)/"[^>]*>(.*?)</a>`)
	reMrdsCardID      = regexp.MustCompile(`(?is)<div\b[^>]*class="[^"]*\bpost-card\b[^"]*"[^>]*\bid="post-card-(\d+)"`)
	reMrdsBanner      = regexp.MustCompile(`(?is)loadBannerDirect\(\s*'([^']*)'\s*,[^)]*#post-card-(\d+)`)
	reMrdsChunk       = regexp.MustCompile(`(?is)(<a\b[^>]*href="/archives/\d+/"[^>]*>.*?</a>)|(<span\b[^>]*class="[^"]*\btime\b[^"]*"[^>]*>[^<]*</span>)`)
	reMrdsTimeSpan    = regexp.MustCompile(`(?is)<span\b[^>]*class="[^"]*\btime\b[^"]*"[^>]*>([^<]*)</span>`)
	reMrdsPageInfo    = regexp.MustCompile(`(?is)class="page-info"[^>]*>\s*(\d+)\s*/\s*(\d+)\s*<`)
	reMrdsPlayer      = regexp.MustCompile(`(?is)<div\b[^>]*class="dplayer"[^>]*>`)
	reMrdsJSONLD      = regexp.MustCompile(`(?is)<script[^>]*type=["'']application/ld\+json["''][^>]*>(.*?)</script>`)
	reMrdsVideoID     = regexp.MustCompile(`data-video_id="([^"]*)"`)
	reMrdsVideoTitle  = regexp.MustCompile(`data-video_title="([^"]*)"`)
	reMrdsTypeName    = regexp.MustCompile(`data-video_type_name="([^"]*)"`)
	reMrdsConfig      = regexp.MustCompile(`data-config='([^']+)'`)
	reMrdsH1          = regexp.MustCompile(`(?is)<h1\b[^>]*>(.*?)</h1>`)
	reMrdsCardTitle   = regexp.MustCompile(`(?is)<h2\b[^>]*class="[^"]*post-card-title[^"]*"[^>]*>(.*?)</h2>`)
	reMrdsTemplateVar = regexp.MustCompile(`\{\{[^}]*\}\}`)
	reMrdsSeqSuffix   = regexp.MustCompile(`\d{3}$`)
	reMrdsScript      = regexp.MustCompile(`(?is)<script\b.*?</script>`)
	reMrdsStyle       = regexp.MustCompile(`(?is)<style\b.*?</style>`)
	reMrdsAnyTag      = regexp.MustCompile(`(?is)<[^>]+>`)
)

// mrdsCardTitle 从一条列表项里取纯标题。
//
// 分类/搜索页的卡片结构是 <a><div class="post-card">…<h2 class="post-card-title">
// 标题</h2><div class="post-card-info">作者 • 日期 • 分类</div>…</div></a>，
// 整个卡片被 <a> 包住，直接取锚文本会把作者、日期、分类一并吸进来；
// 而 <script> 里的 loadBannerDirect 和未渲染的 {{u.username}} 模板也会混进文本。
// 所以优先取 h2.post-card-title，取不到再退化到「剥掉 time/script/占位符后的锚文本」。
func mrdsCardTitle(inner string) string {
	if match := reMrdsCardTitle.FindStringSubmatch(inner); len(match) == 2 {
		if title := mrdsClean(match[1]); title != "" {
			return title
		}
	}
	cleaned := reMrdsScript.ReplaceAllString(inner, " ")
	cleaned = reMrdsTimeSpan.ReplaceAllString(cleaned, " ")
	cleaned = reMrdsTemplateVar.ReplaceAllString(cleaned, " ")
	return mrdsClean(cleaned)
}

func (d *Downloader) fetchMrdsCategories() []nativeCategory {
	return append([]nativeCategory(nil), mrdsCategories...)
}

func validMrdsCategory(category string) bool {
	if category == "" {
		return true
	}
	if len(category) > 64 || strings.ContainsAny(category, "|/\\\x00\r\n ") {
		return false
	}
	for _, entry := range mrdsCategories {
		if entry.ID == category {
			return true
		}
	}
	return false
}

// mrdsNumericID 校验 /archives/{id}/ 的数字 ID。
func mrdsNumericID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > mrdsMaxID {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func mrdsClean(value string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(html.UnescapeString(reMrdsAnyTag.ReplaceAllString(reMrdsStyle.ReplaceAllString(reMrdsScript.ReplaceAllString(value, " "), " "), " "))), " "))
}

func mrdsCover(value string) string {
	value = strings.TrimSpace(html.UnescapeString(value))
	value = strings.Replace(value, `\/`, "/", -1)
	if value == "" {
		return ""
	}
	absolute := false
	switch {
	case strings.HasPrefix(value, "//"):
		value, absolute = "https:"+value, true
	case strings.HasPrefix(value, "http://"), strings.HasPrefix(value, "https://"):
		absolute = true
	case strings.HasPrefix(value, "/"):
		value, absolute = mrdsSiteBaseURL+value, true
	default:
		return ""
	}
	if !absolute {
		return ""
	}
	// 上游封面常带 //upload_01/... 这种多余斜杠，收敛成单个。
	scheme, rest, found := strings.Cut(value, "://")
	if !found {
		return value
	}
	for strings.Contains(rest, "//") {
		rest = strings.ReplaceAll(rest, "//", "/")
	}
	return scheme + "://" + rest
}

func (d *Downloader) mrdsGet(ctx context.Context, path string) (string, error) {
	address := path
	if !strings.HasPrefix(address, "http") {
		address = mrdsSiteBaseURL + path
	}
	pageContext := context.WithValue(ctx, providerTextUserAgentKey{}, "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	return d.fetchProviderText(pageContext, address, mrdsSiteBaseURL+"/")
}

// mrdsPageTotal 从 page-info 读总页数；读不到时按是否满页保守推断。
func mrdsPageTotal(body string, page, count int) int {
	if match := reMrdsPageInfo.FindStringSubmatch(body); len(match) == 3 {
		if total, err := strconv.Atoi(match[2]); err == nil && total > 0 && total <= mrdsMaxPage*10 {
			return total
		}
	}
	if count >= mrdsPageSize {
		return page + 1
	}
	return page
}

// mrdsCardsFromBody 解析列表页。
//
// 列表页有 card 式（分类/搜索，带 post-card 与脚本封面）和 archives 式
// （/archives.html 按月分组的纯文字流，无封面）两种结构，分别解析后合并。
func mrdsCardsFromBody(body string) []Drama {
	covers := map[string]string{}
	for _, match := range reMrdsBanner.FindAllStringSubmatch(body, -1) {
		if cover := mrdsCover(match[1]); cover != "" {
			covers[match[2]] = cover
		}
	}
	cardIDs := map[string]bool{}
	for _, match := range reMrdsCardID.FindAllStringSubmatch(body, -1) {
		cardIDs[match[1]] = true
	}
	// /archives.html 的月视图里 <span class="time">10-22</span> 嵌在 <a> 内部，
	// 所以直接从锚文本里取；取不到再按出现顺序与独立 time 标签配对。
	times := map[string]string{}
	for _, match := range reMrdsArchiveLink.FindAllStringSubmatch(body, -1) {
		if inner := reMrdsTimeSpan.FindStringSubmatch(match[2]); len(inner) == 2 {
			times[match[1]] = mrdsClean(inner[1])
		}
	}
	pending := ""
	for _, chunk := range reMrdsChunk.FindAllStringSubmatch(body, -1) {
		if chunk[2] == "" {
			continue
		}
		if inner := reMrdsTimeSpan.FindStringSubmatch(chunk[2]); len(inner) == 2 {
			pending = mrdsClean(inner[1])
			continue
		}
		pending = mrdsClean(chunk[2])
	}
	for _, match := range reMrdsArchiveLink.FindAllStringSubmatch(body, -1) {
		if times[match[1]] != "" || pending == "" {
			continue
		}
		times[match[1]] = pending
		pending = ""
	}

	items := make([]Drama, 0, mrdsPageSize)
	seen := map[string]bool{}
	for _, match := range reMrdsArchiveLink.FindAllStringSubmatch(body, -1) {
		id := match[1]
		if !mrdsNumericID(id) || seen[id] {
			continue
		}
		title := mrdsCardTitle(match[2])
		if title == "" {
			continue
		}
		seen[id] = true
		remark := times[id]
		if remark == "" && cardIDs[id] {
			remark = "更新"
		}
		cover := covers[id]
		items = append(items, Drama{
			ID: providerDramaID(sourceMrds, id), Source: sourceMrds, SourceID: id,
			Title: truncate(title, mrdsMaxTitle), Name: truncate(title, mrdsMaxTitle),
			Cover: cover, CoverURL: cover, ChannelName: mrdsSiteName,
			CategoryName: mrdsSiteName, Remark: truncate(remark, 64),
		})
		if len(items) >= mrdsPageSize*2 {
			break
		}
	}
	return items
}

func mrdsCategoryPath(category string, page int) string {
	if category == "" {
		// /archives.html 是按月分组的纯文字流，没有任何封面与日期；
		// 默认走主分类页，才能拿到封面。
		if page > 1 {
			return fmt.Sprintf("/category/%s/%d/", mrdsCategories[0].ID, page)
		}
		return "/category/" + mrdsCategories[0].ID + "/"
	}
	if page > 1 {
		return fmt.Sprintf("/category/%s/%d/", url.PathEscape(category), page)
	}
	return "/category/" + url.PathEscape(category) + "/"
}

func mrdsSearchPath(keyword string, page int) string {
	escaped := url.PathEscape(strings.TrimSpace(keyword))
	if escaped == "" {
		return ""
	}
	if page > 1 {
		return fmt.Sprintf("/search/%s/%d/", escaped, page)
	}
	return "/search/" + escaped + "/"
}

func (d *Downloader) fetchMrdsCatalogPage(ctx context.Context, page int, category, query string) ([]Drama, bool, error) {
	if page < 1 || page > mrdsMaxPage {
		return nil, false, errors.New("每日大赛目录页码无效")
	}
	path := ""
	if query != "" {
		path = mrdsSearchPath(query, page)
	} else {
		if !validMrdsCategory(category) {
			return nil, false, errors.New("每日大赛内容分类无效")
		}
		path = mrdsCategoryPath(category, page)
	}
	if path == "" {
		return []Drama{}, false, nil
	}
	body, err := d.mrdsGet(ctx, path)
	if err != nil {
		return nil, false, err
	}
	items := mrdsCardsFromBody(body)
	if len(items) == 0 {
		return []Drama{}, false, nil
	}
	return items, page < mrdsPageTotal(body, page, len(items)), nil
}

// mrdsMeta 是文章页 JSON-LD 里我们关心的部分。
type mrdsMeta struct {
	Headline    string `json:"headline"`
	Description string `json:"description"`
	Published   string `json:"datePublished"`
	Section     string `json:"articleSection"`
	Keywords    string `json:"keywords"`
	Author      struct {
		Name string `json:"name"`
	} `json:"author"`
	Video struct {
		Name     string `json:"name"`
		Duration string `json:"duration"`
	} `json:"video"`
}

// mrdsParseMeta 解析文章页的 JSON-LD。
//
// 结构是 WebPage.mainEntity -> Article；Article.video 是另一个 VideoObject，
// 只带元数据（不含 m3u8），播放地址要从 dplayer 的 data-config 取。
func mrdsParseMeta(body string) (mrdsMeta, bool) {
	for _, block := range reMrdsJSONLD.FindAllStringSubmatch(body, -1) {
		var page struct {
			Type       string    `json:"@type"`
			Headline   string    `json:"headline"`
			DatePub    string    `json:"datePublished"`
			Section    string    `json:"articleSection"`
			MainEntity *mrdsMeta `json:"mainEntity"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(block[1])), &page); err != nil {
			continue
		}
		if page.MainEntity != nil && page.MainEntity.Headline != "" {
			return *page.MainEntity, true
		}
		if page.Type == "Article" || page.Headline != "" {
			return mrdsMeta{
				Headline:  page.Headline,
				Published: page.DatePub,
				Section:   page.Section,
			}, page.Headline != ""
		}
	}
	return mrdsMeta{}, false
}

// mrdsDurationSeconds 解析 ISO-8601 时长。
//
// 上游给的是 PT10M15S / PT1H2M3S 这类形式，必须按「数字 + 单位」逐段读，
// 不能按后缀依次剥离（那样 PT10M15S 会被剥成 10M 而解析失败）。
func mrdsDurationSeconds(value string) time.Duration {
	value = strings.ToUpper(strings.TrimSpace(value))
	if !strings.HasPrefix(value, "PT") {
		return 0
	}
	var total time.Duration
	digits := ""
	for _, r := range value[2:] {
		switch {
		case r >= '0' && r <= '9':
			digits += string(r)
		case r == 'H' || r == 'M' || r == 'S':
			amount, err := strconv.Atoi(digits)
			digits = ""
			if err != nil {
				return 0
			}
			switch r {
			case 'H':
				total += time.Duration(amount) * time.Hour
			case 'M':
				total += time.Duration(amount) * time.Minute
			case 'S':
				total += time.Duration(amount) * time.Second
			}
		default:
			return 0
		}
	}
	return total
}

// mrdsChapterVideo 保存一集的播放信息，key 用 data-video_id。
type mrdsChapterVideo struct {
	ID      string
	Title   string
	Type    string
	URL     string
	URLH265 string
}

// mrdsMediaURL 从 data-config 的 video / video_h265 字段里取地址。
//
// 上游这个字段有时是对象 {"url":...}，有时是空数组 []（表示该清晰度没有），
// 所以不能直接反序列化进结构体，否则空数组会让整块 JSON 解析失败、把
// 另一侧明明可用的地址一起丢掉。这里逐个按 RawMessage 兜住形态差异。
func mrdsMediaURL(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == "[]" || trimmed == "{}" {
		return ""
	}
	var item struct {
		URL  string `json:"url"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return ""
	}
	address := strings.TrimSpace(item.URL)
	address = strings.ReplaceAll(address, `\/`, "/")
	if !strings.HasPrefix(address, "http") {
		return ""
	}
	return address
}

// mrdsParseChapters 解析文章页的所有 dplayer 分集，按 data-video_id 排序返回。
func mrdsParseChapters(body string) []mrdsChapterVideo {
	players := reMrdsPlayer.FindAllStringIndex(body, -1)
	if len(players) == 0 {
		return nil
	}
	out := make([]mrdsChapterVideo, 0, len(players))
	seen := map[string]bool{}
	for index, span := range players {
		start := span[1]
		end := len(body)
		if index+1 < len(players) {
			end = players[index+1][0]
		}
		tag := body[span[0]:start]
		id := ""
		if match := reMrdsVideoID.FindStringSubmatch(tag); len(match) == 2 {
			id = strings.TrimSpace(match[1])
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		entry := mrdsChapterVideo{ID: id}
		if match := reMrdsVideoTitle.FindStringSubmatch(tag); len(match) == 2 {
			entry.Title = mrdsClean(match[1])
		}
		if match := reMrdsTypeName.FindStringSubmatch(tag); len(match) == 2 {
			entry.Type = mrdsClean(match[1])
		}
		// data-config 就在这个 <div class="dplayer" ...> 开始标签内部，
		// 不是标签之后的内容，所以必须在 tag 里找。
		segment := tag
		if !reMrdsConfig.MatchString(segment) {
			segment = body[start:end]
		}
		if match := reMrdsConfig.FindStringSubmatch(segment); len(match) == 2 {
			// video 与 video_h265 单独解析：上游有时给对象、有时给空数组 []，
			// 用 RawMessage 兜住形态差异，避免一边的异常把另一边也带下水。
			var cfg struct {
				Video     json.RawMessage `json:"video"`
				VideoH265 json.RawMessage `json:"video_h265"`
			}
			raw := strings.ReplaceAll(html.UnescapeString(match[1]), `\/`, "/")
			if err := json.Unmarshal([]byte(raw), &cfg); err == nil {
				entry.URL = mrdsMediaURL(cfg.Video)
				entry.URLH265 = mrdsMediaURL(cfg.VideoH265)
			}
		}
		if entry.URL == "" && entry.URLH265 == "" {
			continue
		}
		out = append(out, entry)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (d *Downloader) fetchMrdsDetail(ctx context.Context, sourceID string) (Drama, []Chapter, error) {
	sourceID = strings.TrimSpace(sourceID)
	if !mrdsNumericID(sourceID) {
		return Drama{}, nil, errors.New("每日大赛剧集 ID 无效")
	}
	body, err := d.mrdsGet(ctx, "/archives/"+sourceID+"/")
	if err != nil {
		return Drama{}, nil, err
	}
	videos := mrdsParseChapters(body)
	if len(videos) == 0 {
		return Drama{}, nil, errors.New("每日大赛该文章未提供可用播放地址")
	}

	drama := Drama{
		ID: providerDramaID(sourceMrds, sourceID), Source: sourceMrds, SourceID: sourceID,
		ChannelName: mrdsSiteName, CategoryName: mrdsSiteName,
	}
	meta, hasMeta := mrdsParseMeta(body)
	if hasMeta {
		drama.Title = truncate(meta.Headline, mrdsMaxTitle)
		drama.Desc = truncate(meta.Description, mrdsMaxTitle)
		drama.Intro = drama.Desc
		if meta.Section != "" {
			drama.CategoryName = truncate(meta.Section, 64)
		}
		if meta.Author.Name != "" {
			drama.Tags = []string{truncate(meta.Author.Name, 32)}
		}
	}
	if drama.Title == "" {
		if match := reMrdsH1.FindStringSubmatch(body); len(match) == 2 {
			drama.Title = truncate(mrdsClean(match[1]), mrdsMaxTitle)
		}
	}
	if drama.Title == "" {
		drama.Title = truncate(videos[0].Title, mrdsMaxTitle)
	}
	drama.Name = drama.Title
	drama.TotalEpisode = strconv.Itoa(len(videos))
	drama.EpisodeCount = drama.TotalEpisode

	chapters := make([]Chapter, 0, len(videos))
	for index, video := range videos {
		// data-video_title 末尾带 3 位序号（…001），展示时去掉。
		title := strings.TrimSpace(reMrdsSeqSuffix.ReplaceAllString(video.Title, ""))
		if title == "" {
			title = fmt.Sprintf("第%d集", index+1)
		}
		chapters = append(chapters, Chapter{
			ID:       providerChapterID(sourceMrds, sourceID, video.ID),
			Source:   sourceMrds,
			Title:    truncate(title, 128),
			VideoURL: truncate(fmt.Sprintf("%d", index+1), 8),
			PageURL:  mrdsSiteBaseURL + "/archives/" + sourceID + "/",
			Referer:  mrdsSiteBaseURL + "/",
		})
	}
	return drama, chapters, nil
}


func (d *Downloader) resolveMrdsMedia(ctx context.Context, task Task) (providerMedia, error) {
	source, sourceID, valid := splitProviderDramaID(task.DramaID)
	prefix := providerChapterID(sourceMrds, sourceID, "")
	if !valid || source != sourceMrds || !mrdsNumericID(sourceID) || !strings.HasPrefix(task.Chapter.ID, prefix) {
		return providerMedia{}, errors.New("每日大赛播放分集信息无效，请刷新详情")
	}
	key := strings.TrimSpace(strings.TrimPrefix(task.Chapter.ID, prefix))
	if key == "" {
		return providerMedia{}, errors.New("每日大赛缺少分集标识")
	}
	// auth_key 带时效，每次播放都要重新取文章页，不能缓存地址。
	body, err := d.mrdsGet(ctx, "/archives/"+sourceID+"/")
	if err != nil {
		return providerMedia{}, err
	}
	for _, video := range mrdsParseChapters(body) {
		if video.ID != key {
			continue
		}
		media := providerMedia{Referer: mrdsSiteBaseURL + "/"}
		if video.URL != "" {
			media.Variants = append(media.Variants, providerMedia{URL: video.URL, Referer: media.Referer, Quality: 1080})
		}
		if video.URLH265 != "" {
			media.Variants = append(media.Variants, providerMedia{URL: video.URLH265, Referer: media.Referer, Quality: 2160})
		}
		if len(media.Variants) == 0 {
			return providerMedia{}, errors.New("每日大赛该分集未返回播放地址")
		}
		// 优先 H.265（体积与画质更优），退化到 H.264。
		media.URL = video.URLH265
		if media.URL == "" {
			media.URL = video.URL
			media.Quality = 1080
		} else {
			media.Quality = 2160
		}
		if meta, ok := mrdsParseMeta(body); ok {
			if duration := mrdsDurationSeconds(meta.Video.Duration); duration > 0 {
				media.Duration = duration
			}
		}
		return media, nil
	}
	return providerMedia{}, errors.New("每日大赛未找到该分集，可能已下架")
}
