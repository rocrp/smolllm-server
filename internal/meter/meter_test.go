package meter

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadStatsSkipsMalformedJSONLLines(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	content := `{"ts":"2026-07-03T11:00:00Z","alias":"fast","provider":"groq","input_tokens":3,"output_tokens":2,"duration_ms":10,"ok":true}
{bad-json}
{"ts":"2026-07-03T11:01:00Z","alias":"fast","provider":"groq","input_tokens":4,"output_tokens":1,"duration_ms":20,"ok":false}
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	stats, err := ReadStats(path, 1, now)
	require.NoError(t, err)
	require.Equal(t, 1, stats.SkippedLines)
	require.Len(t, stats.Data, 1)
	require.Equal(t, 2, stats.Data[0].Requests)
	require.Equal(t, 1, stats.Data[0].Errors)
	require.Equal(t, 7, stats.Data[0].InputTokens)
	require.Equal(t, 3, stats.Data[0].OutputTokens)
}
