package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"ragflow/internal/dao"
	"ragflow/internal/entity/models"
	"ragflow/internal/ingestion/archiveartifact"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/tokenizer"
)

type modelDatabaseKey struct{}

func modelDatabase(ctx context.Context) *gorm.DB {
	if db, ok := ctx.Value(modelDatabaseKey{}).(*gorm.DB); ok {
		return db
	}
	return dao.DB
}

// ArchiveEmbeddingModel resolves a pinned reference using the caller's
// transaction and fingerprints the parameters the embedding driver consumes.
// API credentials are intentionally excluded and may rotate between attempts.
func ArchiveEmbeddingModel(ctx context.Context, db *gorm.DB, reference string) (*models.EmbeddingModel, archiveartifact.Embedding, error) {
	var model *models.EmbeddingModel
	var identity archiveartifact.Embedding
	if db == nil {
		return nil, identity, fmt.Errorf("archive model database unavailable")
	}
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		model, identity, err = archiveEmbeddingModel(ctx, tx.Clauses(clause.Locking{Strength: "UPDATE"}), reference)
		return err
	})
	return model, identity, err
}

func archiveEmbeddingModel(ctx context.Context, db *gorm.DB, reference string) (*models.EmbeddingModel, archiveartifact.Embedding, error) {
	ctx = context.WithValue(ctx, modelDatabaseKey{}, db)
	model, err := NewModelFactory().NewEmbeddingModel(ctx, reference)
	if err != nil {
		return nil, archiveartifact.Embedding{}, err
	}
	if model == nil || model.ModelName == nil || model.Info() == nil {
		return nil, archiveartifact.Embedding{}, fmt.Errorf("%w: model identity unavailable", &codearchive.Error{Code: "ZIP_EMBEDDING_CHANGED"})
	}
	parameters := map[string]any{"info": model.Info(), "name": *model.ModelName, "max_tokens": model.ResolveMaxTokens(), "batch_size": model.ResolveBatchSize(), "driver": model.ModelDriver.Name(), "dimension": 0, "encoding_format": "", "query": false}
	parameters["uncounted_ratio_upper"] = tokenizer.DefaultUncountedRatioUpper
	parameters["tokenizer_id"] = model.ResolveTokenizerID()
	if provider := dao.GetModelProviderManager().FindProvider(models.CatalogProviderName(model.Info().ProviderName)); provider != nil {
		endpoints := make(map[string]string, len(provider.URL))
		for region, endpoint := range provider.URL {
			endpoints[region] = archiveEndpoint(endpoint)
		}
		parameters["provider_endpoints"] = endpoints
		parameters["provider_url_suffix"] = provider.URLSuffix
		parameters["provider_class"] = provider.Class
	}
	if model.APIConfig != nil {
		parameters["region"] = model.APIConfig.Region
		if model.APIConfig.BaseURL != nil {
			parameters["base_url"] = archiveEndpoint(*model.APIConfig.BaseURL)
		}
		// Some providers put deployment selection alongside credentials inside
		// their JSON API-key field. Keep only named routing parameters.
		if model.APIConfig.ApiKey != nil {
			routing := make(map[string]string)
			for _, key := range []string{"api_version", "azure_api_version", "deployment_name", "deployment", "resource_name", "endpoint", "azure_endpoint", "base_url", "region", "bedrock_region", "model_id", "project_id", "location", "auth_mode"} {
				value := models.ProviderJSONConfigValue(*model.APIConfig.ApiKey, key)
				if strings.Contains(key, "endpoint") || strings.Contains(key, "url") {
					value = archiveEndpoint(value)
				}
				routing[key] = value
			}
			parameters["provider_parameters"] = routing
		}
	}
	raw, err := json.Marshal(parameters)
	if err != nil {
		return nil, archiveartifact.Embedding{}, err
	}
	hash := sha256.Sum256(raw)
	return model, archiveartifact.Embedding{Reference: reference, Fingerprint: hex.EncodeToString(hash[:]), Parameters: string(raw)}, nil
}

func archiveEndpoint(raw string) string {
	endpoint, err := url.Parse(raw)
	if err != nil {
		return "invalid endpoint"
	}
	endpoint.User = nil
	query := endpoint.Query()
	for key := range query {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "key") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "signature") || lower == "authorization" {
			query.Del(key)
		}
	}
	endpoint.RawQuery = query.Encode()
	endpoint.Fragment = ""
	return endpoint.String()
}
