package meter

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Record struct {
	Timestamp    time.Time `json:"ts"`
	Alias        string    `json:"alias"`
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	ModelName    string    `json:"model_name"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	Estimated    bool      `json:"estimated"`
	DurationMS   int64     `json:"duration_ms"`
	TTFTMS       int64     `json:"ttft_ms"`
	Stream       bool      `json:"stream"`
	OK           bool      `json:"ok"`
	Error        string    `json:"error,omitempty"`
}

type StatsResponse struct {
	Object string        `json:"object"`
	Days   int           `json:"days"`
	Data   []StatsBucket `json:"data"`
}

type StatsBucket struct {
	Day           string `json:"day"`
	Alias         string `json:"alias"`
	Provider      string `json:"provider"`
	Requests      int    `json:"requests"`
	Errors        int    `json:"errors"`
	InputTokens   int    `json:"input_tokens"`
	OutputTokens  int    `json:"output_tokens"`
	TotalTokens   int    `json:"total_tokens"`
	AvgDurationMS int64  `json:"avg_duration_ms"`
}

type aggregate struct {
	StatsBucket
	totalDurationMS int64
}

var appendMu sync.Mutex

func Append(path string, record Record) error {
	if path == "" {
		return nil
	}
	appendMu.Lock()
	defer appendMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create usage dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open usage file: %w", err)
	}
	defer func() { _ = f.Close() }()

	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode usage record: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write usage record: %w", err)
	}
	return nil
}

func ReadStats(path string, days int, now time.Time) (StatsResponse, error) {
	if days <= 0 {
		days = 7
	}
	resp := StatsResponse{Object: "usage_stats", Days: days, Data: []StatsBucket{}}
	if path == "" {
		return resp, nil
	}

	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return resp, nil
	}
	if err != nil {
		return resp, fmt.Errorf("open usage file: %w", err)
	}
	defer func() { _ = f.Close() }()

	cutoff := now.UTC().AddDate(0, 0, -days)
	groups := map[string]*aggregate{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var record Record
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return resp, fmt.Errorf("decode usage record: %w", err)
		}
		if record.Timestamp.IsZero() || record.Timestamp.UTC().Before(cutoff) {
			continue
		}
		day := record.Timestamp.UTC().Format("2006-01-02")
		key := day + "\x00" + record.Alias + "\x00" + record.Provider
		agg := groups[key]
		if agg == nil {
			agg = &aggregate{StatsBucket: StatsBucket{Day: day, Alias: record.Alias, Provider: record.Provider}}
			groups[key] = agg
		}
		agg.Requests++
		if !record.OK {
			agg.Errors++
		}
		agg.InputTokens += record.InputTokens
		agg.OutputTokens += record.OutputTokens
		agg.TotalTokens += record.InputTokens + record.OutputTokens
		agg.totalDurationMS += record.DurationMS
	}
	if err := scanner.Err(); err != nil {
		return resp, fmt.Errorf("scan usage file: %w", err)
	}

	resp.Data = make([]StatsBucket, 0, len(groups))
	for _, agg := range groups {
		if agg.Requests > 0 {
			agg.AvgDurationMS = agg.totalDurationMS / int64(agg.Requests)
		}
		resp.Data = append(resp.Data, agg.StatsBucket)
	}
	sort.Slice(resp.Data, func(i, j int) bool {
		a := resp.Data[i]
		b := resp.Data[j]
		if a.Day != b.Day {
			return a.Day > b.Day
		}
		if a.Alias != b.Alias {
			return a.Alias < b.Alias
		}
		return a.Provider < b.Provider
	})
	return resp, nil
}
