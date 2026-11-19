package codearchive

import (
	"encoding/json"
	"math"
	"testing"
)

func TestParseConfig(t *testing.T) {
	for _, params := range []map[string]any{nil, {}} {
		got, err := ParseConfig(params, 512)
		if err != nil || got != (Config{ChunkTokens: 512}) {
			t.Fatalf("%+v %v", got, err)
		}
	}
	for _, value := range []any{1, int64(2048), float64(123), json.Number("512")} {
		got, err := ParseConfig(map[string]any{"chunk_token_num": value, "code_file_overview": true}, 512)
		if err != nil || !got.Overview || got.ChunkTokens < 1 || got.ChunkTokens > 2048 {
			t.Fatalf("%v: %+v %v", value, got, err)
		}
	}
	for _, params := range []map[string]any{
		{"chunk_token_num": 0}, {"chunk_token_num": 2049}, {"chunk_token_num": 1.5},
		{"chunk_token_num": "512"}, {"chunk_token_num": true}, {"chunk_token_num": nil},
		{"chunk_token_num": math.NaN()}, {"chunk_token_num": math.Inf(1)},
		{"chunk_token_num": json.Number("3.14")}, {"code_file_overview": 1},
		{"code_file_overview": "true"}, {"code_file_overview": nil}, {"max_chunks": 1}, {"chunk_target_ratio": .5},
	} {
		if _, err := ParseConfig(params, 512); Reason(err) != "ZIP_CONFIG_INVALID" {
			t.Fatalf("%+v: %v", params, err)
		}
	}
}

func TestParseConfigUsesDeploymentDefaultOnlyWhenOmitted(t *testing.T) {
	got, err := ParseConfig(nil, 128)
	if err != nil || got.ChunkTokens != 128 {
		t.Fatalf("default: %+v %v", got, err)
	}
	got, err = ParseConfig(map[string]any{"chunk_token_num": 256}, 128)
	if err != nil || got.ChunkTokens != 256 {
		t.Fatalf("override: %+v %v", got, err)
	}
	for _, invalid := range []int{0, -1, 2049} {
		if _, err := ParseConfig(nil, invalid); Reason(err) != "ZIP_CONFIG_INVALID" {
			t.Fatalf("default %d: %v", invalid, err)
		}
	}
}

func TestConfigIntegerNumericRepresentations(t *testing.T) {
	type namedInt int64
	type namedUint uint64
	type namedFloat float64
	type namedNumber json.Number
	for _, tc := range []struct {
		value any
		want  int64
		ok    bool
	}{
		{int(1), 1, true}, {int8(2), 2, true}, {int16(3), 3, true},
		{int32(4), 4, true}, {int64(2048), 2048, true}, {namedInt(512), 512, true},
		{int64(-1), -1, true}, {int64(0), 0, true}, {int64(2049), 2049, true},
		{uint(1), 1, true}, {uint8(2), 2, true}, {uint16(3), 3, true},
		{uint32(4), 4, true}, {uint64(2048), 2048, true}, {namedUint(512), 512, true},
		{uint64(0), 0, true}, {uint64(2049), 0, false}, {^uint64(0), 0, false},
		{float32(1), 1, true}, {float64(2048), 2048, true}, {namedFloat(512), 512, true},
		{float32(1.5), 0, false}, {float64(0), 0, false}, {float64(-1), 0, false},
		{math.Nextafter(1, 0), 0, false}, {math.Nextafter(1, 2), 0, false},
		{math.Nextafter(2048, 2049), 0, false},
		{math.NaN(), 0, false}, {math.Inf(1), 0, false}, {math.Inf(-1), 0, false},
		{json.Number("1"), 1, true}, {json.Number("2048.0"), 2048, true},
		{json.Number("5.12e2"), 512, true}, {json.Number("1.00000000000000001"), 1, true},
		{json.Number("0"), 0, false}, {json.Number("2049"), 0, false},
		{json.Number("1.5"), 0, false}, {json.Number("1e9999"), 0, false},
		{json.Number("NaN"), 0, false}, {json.Number("+Inf"), 0, false},
		{json.Number("-Inf"), 0, false}, {json.Number(""), 0, false}, {json.Number("bad"), 0, false},
		{nil, 0, false}, {(*int)(nil), 0, false}, {new(int), 0, false},
		{uintptr(512), 0, false}, {"512", 0, false}, {namedNumber("512"), 0, false},
		{true, 0, false}, {complex(512, 0), 0, false}, {[]int{512}, 0, false},
	} {
		n, ok := configInteger(tc.value)
		if n != tc.want || ok != tc.ok {
			t.Errorf("%T(%v): integer=(%d, %t), want=(%d, %t)", tc.value, tc.value, n, ok, tc.want, tc.ok)
		}
		got, err := ParseConfig(map[string]any{"chunk_token_num": tc.value}, 128)
		if tc.ok && tc.want >= 1 && tc.want <= 2048 {
			if err != nil || got != (Config{ChunkTokens: int(tc.want)}) {
				t.Errorf("%T(%v): config=%+v error=%v", tc.value, tc.value, got, err)
			}
		} else if err == nil || Reason(err) != "ZIP_CONFIG_INVALID" || got != (Config{}) {
			t.Errorf("%T(%v): invalid config=%+v error=%v", tc.value, tc.value, got, err)
		}
	}
}

func TestIsArchiveUsesUploadFilenameNormalization(t *testing.T) {
	for _, name := range []string{"repo.zip", "REPO.ZIP", " repo.zip "} {
		if !IsArchive(name) {
			t.Fatalf("ZIP admission bypass for %q", name)
		}
	}
	for _, name := range []string{"repo.zip.exe", "repo.gz", "zip"} {
		if IsArchive(name) {
			t.Fatalf("not a ZIP filename: %q", name)
		}
	}
}
