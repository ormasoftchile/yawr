package platformkit

// PlatformKitEntry describes a registered platform kit.
type PlatformKitEntry struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Capabilities []string `json:"provides-capabilities"`
	URL          string   `json:"url,omitempty"`
}

// BuiltinPlatformKits is the built-in registry.
var BuiltinPlatformKits = []PlatformKitEntry{
	{
		Name:    "yawr-mobile-platform",
		Version: "0.1.0",
		Capabilities: []string{
			"capability/camera",
			"capability/location",
			"capability/nfc",
			"capability/biometrics",
			"capability/bluetooth",
			"capability/notifications",
		},
		URL: "https://github.com/ormasoftchile/yawr/runtime-mobile-platform",
	},
}
