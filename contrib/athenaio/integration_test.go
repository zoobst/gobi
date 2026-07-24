//go:build integration

// integration_test.go runs against a real AWS Athena workgroup. Not
// executed in normal `go test` runs. Enable with:
//
//	ATHENAIO_TEST_WORKGROUP=your-workgroup \
//	ATHENAIO_TEST_RESULT_LOCATION=s3://your-bucket/prefix/ \
//	go test -tags=integration ./contrib/athenaio/
//
// Costs: each run submits a real Athena query and downloads the
// result file from S3. Expected data-scanned volume is a few KB
// (the fixture query hits INFORMATION_SCHEMA); cost is negligible
// but not zero. AWS credentials must be available via the standard
// SDK chain (env vars, ~/.aws/credentials, EC2/ECS/EKS role, etc.).
//
// No localstack — Athena's query engine has no local equivalent, so
// this is the only way to exercise the real SDK integration path.

package athenaio

import (
	"context"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/config"
)

func TestIntegration_RawQuery(t *testing.T) {
	wg := os.Getenv("ATHENAIO_TEST_WORKGROUP")
	rl := os.Getenv("ATHENAIO_TEST_RESULT_LOCATION")
	if wg == "" || rl == "" {
		t.Skip("set ATHENAIO_TEST_WORKGROUP and ATHENAIO_TEST_RESULT_LOCATION to run")
	}

	ctx := context.Background()
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		t.Fatalf("LoadDefaultConfig: %v", err)
	}

	c, err := NewClient(ClientConfig{
		AWSConfig:      awsCfg,
		Workgroup:      wg,
		ResultLocation: rl,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Small canned query — hits INFORMATION_SCHEMA, scans ~KB.
	lf, err := c.RawQuery(ctx, "SELECT table_schema, table_name FROM information_schema.tables LIMIT 3")
	if err != nil {
		t.Fatalf("RawQuery: %v", err)
	}
	f, err := lf.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if rows, _ := f.Shape(); rows == 0 {
		t.Errorf("query returned zero rows — expected at least 1")
	}

	stats, ok := StatsFor(lf)
	if !ok {
		t.Fatal("StatsFor missing after RawQuery")
	}
	t.Logf("QueryExecutionID=%s ScannedBytes=%d EngineTime=%s TotalTime=%s",
		stats.QueryExecutionID, stats.ScannedBytes, stats.EngineTime, stats.TotalTime)
	if stats.QueryExecutionID == "" {
		t.Error("empty QueryExecutionID in stats")
	}
}
