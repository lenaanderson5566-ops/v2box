package panel

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/lenaanderson5566-ops/v2box/conf"
)

func TestTrafficSpoolLostAcknowledgementAndRestart(t *testing.T) {
	var first string
	hits := 0
	billed := 0
	seen := map[string]bool{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch trafficBatch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
		}
		if batch.BatchID == "" || len(batch.Records) != 1 {
			t.Error("missing batch contract")
		}
		hits++
		if !seen[batch.BatchID] {
			seen[batch.BatchID] = true
			billed += int(batch.Records[0].Upload)
		}
		if first == "" {
			first = batch.BatchID
		}
		if hits == 1 {
			w.Write([]byte(`{"data":{"accepted":false}}`))
			return
		}
		if batch.BatchID != first {
			t.Error("retry changed batch identity")
		}
		w.Write([]byte(`{"data":{"accepted":true}}`))
	}))
	defer server.Close()
	zero := 0
	cfg := &conf.NodeConfig{NodeType: "v2box", APIHost: server.URL, NodeID: 7, Key: "test", RetryCount: &zero, TrafficSpoolDirectory: t.TempDir()}
	client, err := newTestPanel(t, server, cfg)
	if err != nil {
		t.Fatal(err)
	}
	client.trafficDedup.Store(true)
	retained, err := client.ReportUserTrafficReliable(context.Background(), []UserTraffic{{UID: 1, Upload: 123}})
	if !retained || err == nil {
		t.Fatal("unacknowledged bytes must remain durable")
	}
	files, _ := filepath.Glob(filepath.Join(client.spoolPath(), "*.json"))
	if len(files) != 1 {
		t.Fatal("missing durable batch")
	}
	replacement, err := newTestPanel(t, server, cfg)
	if err != nil {
		t.Fatal(err)
	}
	replacement.trafficDedup.Store(true)
	retained, err = replacement.ReportUserTrafficReliable(context.Background(), nil)
	if retained || err != nil {
		t.Fatalf("replay: %v", err)
	}
	files, _ = filepath.Glob(filepath.Join(client.spoolPath(), "*.json"))
	if len(files) != 0 || hits != 2 || billed != 123 {
		t.Fatal("replay duplicated or lost traffic")
	}
}

func TestTrafficSpoolDowngradeAndStorageFailure(t *testing.T) {
	zero := 0
	hits := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.Write([]byte(`{"data":{"accepted":true}}`)) }))
	defer server.Close()
	cfg := &conf.NodeConfig{NodeType: "v2box", APIHost: server.URL, NodeID: 7, Key: "test", RetryCount: &zero, TrafficSpoolDirectory: t.TempDir()}
	client, _ := newTestPanel(t, server, cfg)
	os.MkdirAll(client.spoolPath(), 0700)
	os.WriteFile(filepath.Join(client.spoolPath(), "pending.json"), []byte(`{}`), 0600)
	retained, err := client.ReportUserTrafficReliable(context.Background(), []UserTraffic{{UID: 1, Upload: 5}})
	if retained || err == nil || hits != 0 {
		t.Fatal("must not replay against old panel")
	}
	file := filepath.Join(t.TempDir(), "not-a-directory")
	os.WriteFile(file, []byte("x"), 0600)
	client.trafficSpoolDirectory = file
	client.trafficDedup.Store(true)
	retained, err = client.ReportUserTrafficReliable(context.Background(), []UserTraffic{{UID: 1, Upload: 5}})
	if retained || err == nil || hits != 0 {
		t.Fatal("failed storage must leave counters owned by caller")
	}
}

func TestTrafficSpoolLegacyCompatibility(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)
		if body["batchId"] != nil {
			t.Error("unnegotiated batch")
		}
		w.Write([]byte(`{"data":{"accepted":true}}`))
	}))
	defer server.Close()
	client, _ := newTestPanel(t, server, &conf.NodeConfig{NodeType: "v2box", APIHost: server.URL, NodeID: 7, TrafficSpoolDirectory: t.TempDir()})
	retained, err := client.ReportUserTrafficReliable(context.Background(), []UserTraffic{{UID: 1, Upload: 5}})
	if retained || err != nil {
		t.Fatal(err)
	}
}

func TestTrafficSpoolBoundedReplayWithoutNewTraffic(t *testing.T) {
	hits := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.Write([]byte(`{"data":{"accepted":true}}`)) }))
	defer server.Close()
	client, _ := newTestPanel(t, server, &conf.NodeConfig{NodeType: "v2box", APIHost: server.URL, NodeID: 7, TrafficSpoolDirectory: t.TempDir()})
	client.trafficDedup.Store(true)
	if err := os.MkdirAll(client.spoolPath(), 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 17; i++ {
		batch := trafficBatch{BatchID: uuid.NewString(), Records: []trafficRecord{{ID: 1, Upload: 1}}}
		raw, _ := json.Marshal(batch)
		if err := os.WriteFile(filepath.Join(client.spoolPath(), batch.BatchID+".json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.ReportUserTrafficReliable(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(client.spoolPath(), "*.json"))
	if hits != 16 || len(files) != 1 {
		t.Fatal("replay tick must be bounded")
	}
	if _, err := client.ReportUserTrafficReliable(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if hits != 17 {
		t.Fatal("remaining traffic not replayed without new traffic")
	}
}
