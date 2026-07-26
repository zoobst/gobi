package athenaio

import "testing"

// TestIsCTASDataKey covers the file-shape matrix athenaio has to
// handle when listing CTAS output. Athena engine v2 in particular
// writes parquets *without* a .parquet extension — the previous
// positive `HasSuffix(key, ".parquet")` filter silently dropped
// every v2 data file. isCTASDataKey uses a negative filter instead.
func TestIsCTASDataKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		// Engine v3 / current-style: .parquet suffix, accepted.
		{"tables/abc/data/00000-0-uuid.parquet", true},
		{"gobi/x/00000_0.parquet", true},

		// Engine v2: no suffix on data files. Must still be accepted.
		{"tables/abc/20240101_120000_00000_bucket-00000", true},
		{"gobi/x/20240101_120000_00001_asdfg_bucket-00000", true},

		// Hive-style output files (typical shape without extension).
		{"prefix/00000_0", true},

		// Iceberg metadata: rejected.
		{"tables/abc/metadata/00000-0.metadata.json", false},
		{"tables/abc/metadata/snap-1234567890.avro", false},
		{"tables/abc/metadata/00000-0-uuid-m0.avro", false},

		// Hive symlink manifest: rejected.
		{"prefix/_symlink_format_manifest/manifest.csv", false},

		// Job-marker + checksum sidecar: rejected.
		{"tables/abc/data/_SUCCESS", false},
		{"tables/abc/data/_committed_1234", false},
		{"tables/abc/data/_started_1234", false},
		{"tables/abc/data/.00000_0.crc", false},

		// Directory marker (zero-byte object with trailing slash).
		{"tables/abc/data/", false},
	}
	for _, tc := range cases {
		if got := isCTASDataKey(tc.key); got != tc.want {
			t.Errorf("isCTASDataKey(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}
