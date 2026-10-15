package dto

import "time"

// Setting is one admin setting with its current value and its definition,
// so the panel can build the form field and check it before saving (§6.3).
type Setting struct {
	Key         string     `json:"key" doc:"Setting key, for example liquidsoap.fade_out_ms. The part before the first dot is the group."`
	Group       string     `json:"group" enum:"station,stream,liquidsoap,rotation,filler,outage,retention,playout,air,import" doc:"Settings page the key belongs to."`
	Description string     `json:"description" doc:"English description for developers; the panel shows its own help texts."`
	Type        string     `json:"type" enum:"integer,number,string,boolean,object" doc:"JSON type of value."`
	Unit        string     `json:"unit,omitempty" doc:"Unit of a number, for example ms, seconds, kbps."`
	Value       any        `json:"value" doc:"Current value: the saved value, or the default when is_default is true."`
	Default     any        `json:"default" doc:"Value used until one is saved."`
	IsDefault   bool       `json:"is_default" doc:"True when no value is saved."`
	Nullable    bool       `json:"nullable" doc:"True when null is a valid value."`
	Minimum     *float64   `json:"minimum,omitempty" doc:"Smallest allowed number, or shortest string length."`
	Maximum     *float64   `json:"maximum,omitempty" doc:"Largest allowed number, or longest string length."`
	Enum        []any      `json:"enum,omitempty" nullable:"false" doc:"Allowed values, when limited."`
	Apply       string     `json:"apply" enum:"immediate,reconnect" doc:"When a saved value takes effect. reconnect: when the connection it configures is opened again; changing the stream server briefly interrupts the stream."`
	AffectsAir  bool       `json:"affects_air" doc:"True when a change affects what goes to air; such changes are logged with their UTC time."`
	UpdatedAt   *time.Time `json:"updated_at,omitempty" doc:"When the value was saved; absent for a default."`
}

// SettingList is every setting, grouped in the order of the settings pages.
type SettingList struct {
	Items []Setting `json:"items" nullable:"false"`
}

// SetSetting is the body of PUT /api/settings/{key}.
type SetSetting struct {
	Value any `json:"value" required:"true" doc:"New value; its type and limits are those of the setting (GET /api/settings)."`
}

// SecretStatus says whether a secret is set. The value is never returned.
type SecretStatus struct {
	Name        string `json:"name" doc:"Environment variable that holds the secret."`
	Group       string `json:"group" enum:"station,stream,liquidsoap,rotation,filler,outage,retention,playout,air,import"`
	Description string `json:"description"`
	IsSet       bool   `json:"is_set"`
}

// SecretList is every secret the backend uses.
type SecretList struct {
	Items []SecretStatus `json:"items" nullable:"false"`
}
