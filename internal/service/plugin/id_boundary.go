package plugin

import (
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/service/plugin/pluginid"
)

func parseWirePluginID(value string) (string, model.PluginType, error) {
	parsed, err := pluginid.Parse(value)
	if err != nil {
		return "", "", ErrInvalidRequest
	}
	storageID, ok := parsed.StorageID()
	if !ok {
		// Embedded design addresses do not identify a separate unified Plugin row.
		return "", "", ErrInvalidRequest
	}
	typ, ok := pluginTypeForKind(parsed.Kind)
	if !ok {
		return "", "", ErrInvalidRequest
	}
	return storageID, typ, nil
}

func pluginTypeForKind(kind pluginid.Kind) (model.PluginType, bool) {
	switch kind {
	case pluginid.Expert:
		return model.PluginTypeExpert, true
	case pluginid.ExpertTeam:
		return model.PluginTypeExpertTeam, true
	case pluginid.Skill:
		return model.PluginTypeSkill, true
	case pluginid.Connector:
		return model.PluginTypeConnector, true
	default:
		return "", false
	}
}
