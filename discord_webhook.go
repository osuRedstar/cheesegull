// discord_webhook.go
//
// Go port of osu-discord-webhook/index.js (by Skyreach, MIT).
//
// Differences from the original (intentional, minimal):
//   - config.id + config.token  ->  one webhook URL (DISCORD_WEBHOOK_URL / --discord-webhook)
//   - config.key                ->  cheesegull's osu! API key (OSU_API_KEY / -k)
//   - worldclockapi.com         ->  local UTC clock (the service is dead and it
//                                   was one extra HTTP request every 30 seconds)
//   - osu! API HTTP 429         ->  back off instead of hammering the API
//   - sendEmbed calls           ->  sent one after another (500ms apart) instead of
//                                   all at once, to avoid request bursts
//
// The worker only starts when a webhook URL is configured, runs in its own
// goroutine, and uses its own http.Client, so it never touches cheesegull's
// database, cache, osuapi client or HTTP server.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//

type webhookPayload struct {
	Embeds []webhookEmbed `json:"embeds"`
}

type webhookEmbed struct {
	Title       string         `json:"title,omitempty"`
	URL         string         `json:"url,omitempty"`
	Description string         `json:"description,omitempty"`
	Color       int            `json:"color"`
	Author      *webhookAuthor `json:"author,omitempty"`
	Thumbnail   *webhookMedia  `json:"thumbnail,omitempty"`
	Image       *webhookMedia  `json:"image,omitempty"`
	Fields      []webhookField `json:"fields,omitempty"`
	Footer      *webhookFooter `json:"footer,omitempty"`
}

type webhookAuthor struct {
	Name    string `json:"name"`
	URL     string `json:"url,omitempty"`
	IconURL string `json:"icon_url,omitempty"`
}

type webhookMedia struct {
	URL string `json:"url"`
}

type webhookField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type webhookFooter struct {
	Text    string `json:"text"`
	IconURL string `json:"icon_url,omitempty"`
}

// osu! API v1 get_beatmaps item. All fields are kept as strings, exactly like
// the JS version received them (JSON null -> "").
type webhookBeatmap struct {
	BeatmapsetID     string `json:"beatmapset_id"`
	Approved         string `json:"approved"`
	ApprovedDate     string `json:"approved_date"`
	TotalLength      string `json:"total_length"`
	Version          string `json:"version"`
	Mode             string `json:"mode"`
	Artist           string `json:"artist"`
	Title            string `json:"title"`
	Creator          string `json:"creator"`
	CreatorID        string `json:"creator_id"`
	BPM              string `json:"bpm"`
	DifficultyRating string `json:"difficultyrating"`
}

var (
	webhookSeenMaps  = map[string]bool{}
	webhookFirstInit = 1
	webhookHTTP      = &http.Client{Timeout: 15 * time.Second}
)

func webhookSleep(millis int) {
	time.Sleep(time.Duration(millis) * time.Millisecond)
}

func setupExitHook(webhookURL string) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		sendMessage(webhookURL, "Can't connect to the application!")
		os.Exit(1)
	}()
}

func toMMSS(s string) string {
	secNum, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return "NaN:NaN"
	}
	minutes := secNum / 60
	seconds := secNum - (minutes * 60)
	return fmt.Sprintf("%02d:%02d", minutes, seconds)
}

// moment(x).format('MMMM Do YYYY, h:mm:ss a')
// moment parses "YYYY-MM-DD HH:mm:ss" as-is and formats it back without
// conversion, so no timezone math is done here either.
func formatMoment(raw string) string {
	t, err := time.Parse("2006-01-02 15:04:05", raw)
	if err != nil {
		return "Invalid date"
	}
	day := t.Day()
	suffix := "th"
	if day%100 < 11 || day%100 > 13 {
		switch day % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return fmt.Sprintf("%s %d%s %d, %s", t.Format("January"), day, suffix, t.Year(), t.Format("3:04:05 pm"))
}

// rgb converts discord.js .setColor([r,g,b]) to Discord's integer color.
func rgb(r, g, b int) int {
	return r<<16 | g<<8 | b
}

// hook.send(embed)
func hookSend(webhookURL string, embed webhookEmbed) error {
	body, err := json.Marshal(webhookPayload{Embeds: []webhookEmbed{embed}})
	if err != nil {
		return err
	}
	resp, err := webhookHTTP.Post(webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("discord webhook HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// osuGetBeatmaps does axios.get('https://osu.ppy.sh/api/get_beatmaps?...').
// Returns errOsu429 when the osu! API rate-limits us.
var errOsu429 = fmt.Errorf("osu! API returned HTTP 429 (Too Many Requests)")

func osuGetBeatmaps(rawURL string) ([]webhookBeatmap, error) {
	resp, err := webhookHTTP.Get(rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, errOsu429
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("osu! API HTTP %d: %s", resp.StatusCode, string(body))
	}
	var data []webhookBeatmap
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	return data, nil
}

func sendEmbed(apiKey, webhookURL, bmID string) error {
	data, err := osuGetBeatmaps("https://osu.ppy.sh/api/get_beatmaps?k=" + apiKey + "&s=" + bmID)
	if err != nil {
		log.Println("[W]", err)
		return err
	}
	if len(data) == 0 {
		return nil
	}

	diffFormatted := ""

	// NOTE: the JS compares the raw strings, so "10.5" sorts below "9.2". Kept as-is.
	sort.SliceStable(data, func(i, j int) bool { return data[i].DifficultyRating > data[j].DifficultyRating })
	sort.SliceStable(data, func(i, j int) bool { return data[i].Mode > data[j].Mode })
	for _, nameMap := range data {
		dr, _ := strconv.ParseFloat(nameMap.DifficultyRating, 64)
		diffFormatted += "`" + strconv.FormatFloat(math.Round(dr*100)/100, 'f', 2, 64) + "☆ "

		mode, _ := strconv.Atoi(nameMap.Mode)
		switch mode {
		case 0:
			diffFormatted += "[osu!standard] "
		case 1:
			diffFormatted += "[osu!taiko] "
		case 2:
			diffFormatted += "[osu!catch] "
		case 3:
			diffFormatted += "[osu!mania] "
		default:
			diffFormatted += "[osu!] "
		}
		diffFormatted += nameMap.Version + "`\n"
	}

	var approvalType, thumbnail string
	approved, _ := strconv.Atoi(data[0].Approved)
	switch approved {
	case -2:
		approvalType = " graveyarded"
		thumbnail = "https://i.imgur.com/1k2YqGp.png"
	case -1:
		approvalType = " WIP"
		thumbnail = "https://i.imgur.com/1k2YqGp.png"
	case 0:
		approvalType = " pending"
		thumbnail = "https://i.imgur.com/1k2YqGp.png"
	case 1:
		approvalType = " ranked"
		thumbnail = "https://i.imgur.com/hfdujvi.png"
	case 2:
		approvalType = " approved"
		thumbnail = "https://i.imgur.com/lqsQe0T.png"
	case 3:
		approvalType = " qualified"
		thumbnail = "https://i.imgur.com/lqsQe0T.png"
	case 4:
		approvalType = " loved"
		thumbnail = "https://i.imgur.com/R7dFUL5.png"
	default:
		approvalType = ""
		thumbnail = "https://i.imgur.com/1k2YqGp.png"
	}

	d := data[0]
	embed := webhookEmbed{
		Title: d.Artist + " - " + d.Title,
		Author: &webhookAuthor{
			Name:    "New" + approvalType + " beatmap by " + d.Creator,
			IconURL: "http://s.ppy.sh/a/" + d.CreatorID,
			URL:     "http://osu.ppy.sh/users/" + d.CreatorID,
		},
		URL:         "http://osu.ppy.sh/beatmapsets/" + bmID,
		Description: "",
		Fields: []webhookField{
			{Name: "Set information: ", Value: "`Song length: " + toMMSS(d.TotalLength) + "`\n`" + "BPM: " + d.BPM + "`", Inline: false},
			{Name: "Difficulties: ", Value: diffFormatted, Inline: false},
			{Name: "Direct download: ", Value: " [Official server](https://osu.ppy.sh/beatmapsets/" + bmID + "/download) | [Redstar mirror](https://redstar.moe/d/" + bmID + ") | [Nerinyan mirror](https://nerinyan.moe/d/" + bmID + ") | [Bloodcat mirror](https://bloodcat.com/osu/s/" + bmID + ")\n", Inline: false},
		},
		Color:     rgb(229, 119, 166),
		Thumbnail: &webhookMedia{URL: thumbnail},
		Image:     &webhookMedia{URL: "https://assets.ppy.sh/beatmaps/" + bmID + "/covers/cover.jpg"},
		Footer: &webhookFooter{
			Text:    "Approved at " + formatMoment(d.ApprovedDate) + " [UTC]",
			IconURL: "https://i.imgur.com/h87iocW.png",
		},
	}

	if err := hookSend(webhookURL, embed); err != nil {
		log.Println("[W]", err)
		return err
	}
	log.Printf("[W] Sent beatmapset %s (%s - %s)\n", bmID, d.Artist, d.Title)
	return nil
}

func sendMessage(webhookURL, message string) {
	embed := webhookEmbed{
		Author: &webhookAuthor{Name: message},
		Color:  rgb(119, 199, 229),
	}
	if err := hookSend(webhookURL, embed); err != nil {
		log.Println("[W]", err)
	}
}

// StartDiscordWebhook is called from main() as a goroutine.
func StartDiscordWebhook(apiKey, webhookURL string) {
	webhookURL = strings.TrimSpace(webhookURL)
	if webhookURL == "" || apiKey == "" {
		log.Println("[W] Discord webhook disabled (webhook URL or osu! API key missing)")
		return
	}
	setupExitHook(webhookURL)
	startDiscordWebhookLoop(apiKey, webhookURL)
}

func startDiscordWebhookLoop(apiKey, webhookURL string) {
	for {
		log.Println("[W] Checking for new maps...")

		currDate := time.Now().UTC().Format("2006-01-02")

		data, err := osuGetBeatmaps("https://osu.ppy.sh/api/get_beatmaps?k=" + apiKey + "&a=0&limit=100&since=" + currDate + "%2000:00:00")
		if err == errOsu429 {
			log.Println("[W]", err, "- waiting 60 seconds")
			webhookSleep(60000)
			continue
		}
		if err != nil {
			log.Println("[W]", err)
		} else {
			var grouped []string
			inGroup := map[string]bool{}
			for _, bm := range data {
				if !inGroup[bm.BeatmapsetID] {
					inGroup[bm.BeatmapsetID] = true
					grouped = append(grouped, bm.BeatmapsetID)
				}
			}

			var toSend []string
			for _, set := range grouped {
				if !webhookSeenMaps[set] {
					webhookSeenMaps[set] = true
					if webhookFirstInit == 0 {
						toSend = append(toSend, set)
					}
				}
			}
			if len(toSend) > 0 {
				go func(ids []string) {
					for _, id := range ids {
						if sendEmbed(apiKey, webhookURL, id) == errOsu429 {
							webhookSleep(60000)
						}
						webhookSleep(500)
					}
				}(toSend)
			}

			if webhookFirstInit == 1 {
				sendMessage(webhookURL, "Connected successfully!")
				webhookFirstInit = 0
			}
		}

		webhookSleep(30000)
	}
}

// loadDotEnv reads KEY=VALUE lines from a .env file into the process
// environment so kingpin's .Envar() flags pick them up. Variables already set
// in the real environment win. Missing file = no-op.
func loadDotEnv(filename string) {
	f, err := os.Open(filename)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}
