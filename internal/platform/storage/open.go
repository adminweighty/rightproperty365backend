package storage

import (
	"fmt"
	"log/slog"

	"rightproperty365.com/backend/internal/platform/config"
)

// Open builds the storage selected by the configuration and logs one line
// naming it. The secret key is never logged.
func Open(cfg config.Config, logger *slog.Logger) (Storage, error) {
	switch cfg.StorageDriver {
	case config.StorageDriverS3:
		s, err := NewS3(S3Config{
			Endpoint: cfg.S3Endpoint, Bucket: cfg.S3Bucket, AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey.Reveal(),
			Region: cfg.S3Region, UsePathStyle: cfg.S3UsePathStyle,
		})
		if err != nil {
			return nil, err
		}
		logger.Info("file storage selected: s3 (private bucket; files are only served through the API)",
			slog.String("storage_driver", DriverS3), slog.String("s3_bucket", cfg.S3Bucket), slog.String("s3_region", cfg.S3Region),
			slog.Bool("s3_path_style", cfg.S3UsePathStyle), slog.Bool("s3_secret_key_set", cfg.S3SecretKey.IsSet()))
		return s, nil
	case config.StorageDriverLocal:
		l, err := NewLocal(cfg.StorageLocalDir)
		if err != nil {
			return nil, err
		}
		logger.Warn("file storage selected: local (development only; files are written to this machine's disk)",
			slog.String("storage_driver", DriverLocal), slog.String("storage_dir", l.Dir()))
		return l, nil
	}
	return nil, fmt.Errorf("storage: unknown driver %q", cfg.StorageDriver)
}
