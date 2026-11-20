package kiro

import (
	"encoding/json"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/models"
	"github.com/AkkunYo/kiro4cpa-plugin/internal/wire"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const catalogKey = "kiro4cpa_models" // Removed live catalog cache, cleared on token refresh.

func handleModelForAuth(request []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	var plan string
	if fields, err := decodeFields(req.StorageJSON); err == nil {
		plan, _ = fields["subscriptionTitle"].(string)
	}
	infos := withoutModelExclusions(models.GetModelInfosForPlan(plan), req)
	return wire.OK(pluginapi.ModelResponse{Provider: models.ProviderName, Models: infos}), nil
}
