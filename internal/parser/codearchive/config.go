package codearchive

import (
	"encoding/json"
	"math"
	"reflect"
)

// Config is the entire caller-editable CodeArchive configuration. Resource
// limits, transforms and tokenizers are supplied by the server through Options.
type Config struct {
	ChunkTokens int  `json:"chunk_token_num"`
	Overview    bool `json:"code_file_overview"`
}

func ParseConfig(params map[string]any, defaultChunkTokens int) (Config, error) {
	if defaultChunkTokens < 1 || defaultChunkTokens > 2048 {
		return Config{}, fail("ZIP_CONFIG_INVALID")
	}
	config := Config{ChunkTokens: defaultChunkTokens}
	for key, value := range params {
		switch key {
		case "chunk_token_num":
			n, ok := configInteger(value)
			if !ok || n < 1 || n > 2048 {
				return Config{}, fail("ZIP_CONFIG_INVALID")
			}
			config.ChunkTokens = int(n)
		case "code_file_overview":
			flag, ok := value.(bool)
			if !ok {
				return Config{}, fail("ZIP_CONFIG_INVALID")
			}
			config.Overview = flag
		default:
			return Config{}, fail("ZIP_CONFIG_INVALID")
		}
	}
	return config, nil
}

func configInteger(value any) (int64, bool) {
	if number, ok := value.(json.Number); ok {
		n, err := number.Float64()
		if err != nil {
			return 0, false
		}
		value = n
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n := v.Uint()
		if n > 2048 {
			return 0, false
		}
		return int64(n), true
	case reflect.Float32, reflect.Float64:
		n := v.Float()
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 1 || n > 2048 || math.Trunc(n) != n {
			return 0, false
		}
		return int64(n), true
	}
	return 0, false
}
