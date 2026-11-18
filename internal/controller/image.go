package controller

const (
	// EnvDevicePluginImage names the environment variable holding the
	// device-plugin container image reference.
	EnvDevicePluginImage = "VCR_DEVICE_PLUGIN_IMAGE"

	// DefaultDevicePluginImage is the placeholder device-plugin image used when
	// EnvDevicePluginImage is unset. It is a public placeholder, never a private
	// registry, and every deployment is expected to override it.
	DefaultDevicePluginImage = "quay.io/opendatahub/vllm-vcr-device-plugin:v0.33.0"
)

// DevicePluginImageFromEnv resolves the device-plugin image reference from the
// environment, falling back to DefaultDevicePluginImage.
func DevicePluginImageFromEnv(getenv func(string) string) string {
	if value := getenv(EnvDevicePluginImage); value != "" {
		return value
	}
	return DefaultDevicePluginImage
}
