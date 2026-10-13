package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/settings"
)

// Settings reads and saves the admin settings.
type Settings interface {
	List(ctx context.Context) ([]settings.Setting, error)
	Set(ctx context.Context, key string, value json.RawMessage) (settings.Setting, error)
	Secrets() []settings.SecretStatus
}

type settingListOutput struct {
	Body dto.SettingList
}

type setSettingInput struct {
	Key  string `path:"key" maxLength:"100" doc:"Setting key, for example liquidsoap.fade_out_ms."`
	Body dto.SetSetting
}

type settingOutput struct {
	Body dto.Setting
}

type secretListOutput struct {
	Body dto.SecretList
}

func registerSettings(api huma.API, log *slog.Logger, svc Settings) {
	register(api, log, huma.Operation{
		OperationID: "list-settings",
		Method:      http.MethodGet,
		Path:        "/api/settings",
		Summary:     "List settings",
		Description: "Every admin setting with its value, default, limits, and apply mode. A setting that was never saved has its code default and is_default true. Secrets are not listed (see GET /api/settings/secrets).",
		Tags:        []string{"Settings"},
		Errors:      commonErrors,
	}, func(ctx context.Context, _ *struct{}) (*settingListOutput, error) {
		all, err := svc.List(ctx)
		if err != nil {
			return nil, err
		}
		out := &settingListOutput{Body: dto.SettingList{Items: make([]dto.Setting, 0, len(all))}}
		for _, s := range all {
			out.Body.Items = append(out.Body.Items, settingDTO(s))
		}
		return out, nil
	})

	register(api, log, huma.Operation{
		OperationID: "set-setting",
		Method:      http.MethodPut,
		Path:        "/api/settings/{key}",
		Summary:     "Save a setting",
		Description: "Validates and saves one value. A refused value returns a field error on value. Secrets cannot be set here (setting_not_editable): they live in the environment.",
		Tags:        []string{"Settings"},
		Errors:      append([]int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity}, commonErrors...),
	}, func(ctx context.Context, in *setSettingInput) (*settingOutput, error) {
		raw, err := json.Marshal(in.Body.Value)
		if err != nil {
			return nil, fmt.Errorf("encode setting value: %w", err)
		}
		s, err := svc.Set(ctx, in.Key, raw)
		if err != nil {
			return nil, err
		}
		return &settingOutput{Body: settingDTO(s)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "list-secrets",
		Method:      http.MethodGet,
		Path:        "/api/settings/secrets",
		Summary:     "List secrets",
		Description: "Whether each secret is set in the environment. Values are never returned; a secret is changed in the environment and takes effect after a restart.",
		Tags:        []string{"Settings"},
		Errors:      commonErrors,
	}, func(context.Context, *struct{}) (*secretListOutput, error) {
		secrets := svc.Secrets()
		out := &secretListOutput{Body: dto.SecretList{Items: make([]dto.SecretStatus, 0, len(secrets))}}
		for _, s := range secrets {
			out.Body.Items = append(out.Body.Items, dto.SecretStatus{
				Name:        s.Definition.EnvName,
				Group:       string(s.Definition.Group),
				Description: s.Definition.Description,
				IsSet:       s.IsSet,
			})
		}
		return out, nil
	})
}

func settingDTO(s settings.Setting) dto.Setting {
	d := s.Definition
	out := dto.Setting{
		Key:         d.Key,
		Group:       string(d.Group),
		Description: d.Description,
		Type:        string(d.Type),
		Unit:        d.Unit,
		Value:       s.Value,
		Default:     d.Default,
		IsDefault:   s.IsDefault,
		Nullable:    d.Nullable,
		Minimum:     d.Minimum,
		Maximum:     d.Maximum,
		Enum:        d.Enum,
		Apply:       string(d.Apply),
		AffectsAir:  d.AffectsAir,
	}
	if s.UpdatedAt != nil {
		t := s.UpdatedAt.UTC()
		out.UpdatedAt = &t
	}
	return out
}
