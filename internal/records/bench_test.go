package records

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
)

// BenchmarkIngestLiteLLMBatch measures durable ingest of LiteLLM batches
// (512 records, the logger's default batch size) with a real fsync per
// batch, as a gateway would drive it.
func BenchmarkIngestLiteLLMBatch(b *testing.B) {
	sp, err := spool.Open(filepath.Join(b.TempDir(), "records"), spool.Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer sp.Close()
	formats, _ := LoadFormats("", true)
	in := NewIngester(sp, formats, Options{Window: time.Minute}, nil, nil, nil)
	now := time.Now().Unix()
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < 512; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"id":"req-%%d-%d","status":"success","model_group":"llama-70b","prompt_tokens":512,"completion_tokens":128,"endTime":%d,"metadata":{"user_api_key_team_id":"acme"}}`, i, now)
	}
	sb.WriteString("]")
	tmpl := sb.String()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		body := strings.ReplaceAll(tmpl, "%d", fmt.Sprint(n))
		res, err := in.Ingest("litellm", "bench", "", []byte(body))
		if err != nil || res.Accepted != 512 {
			b.Fatalf("%+v %v", res, err)
		}
	}
	b.ReportMetric(float64(512*b.N)/b.Elapsed().Seconds(), "records/s")
}
