package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// updateRepo is the single place the app looks for new releases.
const updateRepo = "Vick-moo/VKMDownload"

// UpdateInfo describes the newest published release compared to this build.
type UpdateInfo struct {
	Available bool   `json:"available"`
	Version   string `json:"version"`
	URL       string `json:"url"`
	Current   string `json:"current"`
}

// isNewerVersion reports whether tag (e.g. "v0.2.3") is newer than current
// (e.g. "0.2.2"). It compares dot-separated numeric parts only, so a tag the
// project never produced can never read as newer by accident.
func isNewerVersion(current, tag string) bool {
	num := func(s string) []int {
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V"))
		var out []int
		for _, p := range strings.Split(s, ".") {
			digits := ""
			for _, r := range p {
				if r < '0' || r > '9' {
					break
				}
				digits += string(r)
			}
			if digits == "" {
				return nil
			}
			n, err := strconv.Atoi(digits)
			if err != nil {
				return nil
			}
			out = append(out, n)
		}
		return out
	}

	a, b := num(current), num(tag)
	if a == nil || b == nil {
		return false
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		if b[i] != a[i] {
			return b[i] > a[i]
		}
	}
	return len(b) > len(a)
}

// CheckUpdate asks GitHub for the latest release. It is a binding so the
// settings page can offer a manual "check" alongside the automatic one at
// startup. A missing network is not an error worth showing: it just reports
// no update.
func (a *App) CheckUpdate() (UpdateInfo, error) {
	info := UpdateInfo{Current: appVersion}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/"+updateRepo+"/releases/latest", nil)
	if err != nil {
		return info, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return info, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info, fmt.Errorf("releases API answered %d", resp.StatusCode)
	}

	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return info, err
	}
	if isNewerVersion(appVersion, rel.TagName) {
		info.Available = true
		info.Version = strings.TrimPrefix(strings.TrimPrefix(rel.TagName, "v"), "V")
		info.URL = rel.HTMLURL
	}
	return info, nil
}

// checkUpdateOnce runs at startup and announces a newer release, if any.
// Silence on failure: an offline machine must start exactly as fast as an
// online one, and the settings page offers a manual check anyway.
func (a *App) checkUpdateOnce() {
	info, err := a.CheckUpdate()
	if err != nil || !info.Available {
		return
	}
	a.emit("update:available", map[string]string{
		"version": info.Version,
		"url":     info.URL,
	})
}

// OpenReleasePage opens the release page in the system browser. The host is
// allow-listed: a binding that opened any URL it was given would turn every
// stored string into a potential lure.
func (a *App) OpenReleasePage(url string) error {
	u := strings.TrimSpace(url)
	if !strings.HasPrefix(u, "https://github.com/"+updateRepo+"/releases/") {
		return fmt.Errorf("只允许打开本项目的发布页")
	}
	runtime.BrowserOpenURL(a.ctx, u)
	return nil
}
