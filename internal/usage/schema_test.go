package usage

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
	"time"
)

// TestPublishedSchemaMatchesEvent keeps docs/schema/usage-event.v1.json and
// the Event struct in lockstep: same field names, same required set, same
// schema version, and a real event satisfies the documented constraints.
func TestPublishedSchemaMatchesEvent(t *testing.T) {
	b, err := os.ReadFile("../../docs/schema/usage-event.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatal(err)
	}
	var version struct {
		Const string `json:"const"`
	}
	json.Unmarshal(schema.Properties["schema_version"], &version)
	if version.Const != SchemaVersion {
		t.Fatalf("schema const %q != SchemaVersion %q", version.Const, SchemaVersion)
	}

	ws := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	e := Event{Tenant: "acme", Metric: MetricGPUHours, Quantity: 1, WindowStart: ws, WindowEnd: ws.Add(time.Minute),
		Region: "r", Project: "p", SKU: "s", ResourceID: "x", Source: "collector", RecordedAt: ws,
		Dimensions: map[string]string{DimCapacityType: CapacitySpot}, Properties: map[string]any{"k": 1}}
	e.Finalize()
	raw, _ := json.Marshal(e)
	var fields map[string]any
	json.Unmarshal(raw, &fields)

	var structKeys, schemaKeys []string
	for k := range fields {
		structKeys = append(structKeys, k)
		if _, ok := schema.Properties[k]; !ok {
			t.Errorf("Event field %q is not documented in the schema", k)
		}
	}
	for k := range schema.Properties {
		schemaKeys = append(schemaKeys, k)
		if _, ok := fields[k]; !ok {
			t.Errorf("schema property %q does not exist on Event", k)
		}
	}
	for _, r := range schema.Required {
		if _, ok := fields[r]; !ok {
			t.Errorf("required schema field %q missing from a finalized event", r)
		}
	}
	sort.Strings(structKeys)
	sort.Strings(schemaKeys)
	if t.Failed() {
		t.Logf("struct: %v\nschema: %v", structKeys, schemaKeys)
	}
}
