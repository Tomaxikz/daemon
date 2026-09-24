package config

// Path is the local configuration file used to initialize this instance.
func (c *Configuration) Path() string {
	return c.path
}

type NetworkPolicyConfiguration struct {
	Enabled        bool   `default:"true" yaml:"enabled"`
	IPv6           bool   `yaml:"ipv6"`
	MaxUploadBPS   int64  `yaml:"max_upload_bps"`
	MaxDownloadBPS int64  `yaml:"max_download_bps"`
	Runtime        string `yaml:"runtime"`
}
