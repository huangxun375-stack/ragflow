package server

import (
	"fmt"
	"time"
)

type CodeArchiveCleanupConfig struct {
	Interval  time.Duration
	Grace     time.Duration
	BatchSize int
}

func GetCodeArchiveCleanupConfig() (CodeArchiveCleanupConfig, error) {
	config := CodeArchiveCleanupConfig{Interval: 5 * time.Minute, Grace: 10 * time.Minute, BatchSize: 100}
	if globalViper == nil {
		return config, nil
	}
	for key, target := range map[string]*time.Duration{"interval_seconds": &config.Interval, "grace_seconds": &config.Grace} {
		path := "code_archive_cleanup." + key
		if globalViper.IsSet(path) {
			seconds := globalViper.GetInt64(path)
			if seconds < 1 || seconds > 86400 {
				return config, fmt.Errorf("%s must be between 1 and 86400", path)
			}
			*target = time.Duration(seconds) * time.Second
		}
	}
	if globalViper.IsSet("code_archive_cleanup.batch_size") {
		config.BatchSize = globalViper.GetInt("code_archive_cleanup.batch_size")
	}
	if config.BatchSize < 1 || config.BatchSize > 10000 {
		return config, fmt.Errorf("code_archive_cleanup.batch_size must be between 1 and 10000")
	}
	return config, nil
}
