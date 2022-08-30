package hcsshim

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Microsoft/hcsshim/internal/hcs"
	hcsschema "github.com/Microsoft/hcsshim/internal/schema2"
)

type SupportedSchemaVersionsStruct struct {
	SupportedSchemaVersions []hcsschema.Version `json:"SupportedSchemaVersions,omitempty"`
}

func GetHcsSchemaVersion() (schemaVersion *hcsschema.Version, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	serviceProperties, err := hcs.GetServiceProperties(ctx, hcsschema.PropertyQuery{PropertyTypes: []string{"Basic"}})
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve HCS schema version: %s", err)
	}

	supportedSchemaVersions := &SupportedSchemaVersionsStruct{}
	if err := json.Unmarshal(serviceProperties.Properties[0], &supportedSchemaVersions); err != nil {
		return nil, fmt.Errorf("failed to unmarshal HCS Schema Version: %s", err)
	}

	schemaVersion = &supportedSchemaVersions.SupportedSchemaVersions[0]

	return
}
