// Package migration moves a docker-compose stack from one VPS to another:
// compose file + env files + named-volume contents. Source state is never
// destroyed by this package — bringing the source down is the user's
// decision, made after they've verified the target.
package migration

// Plan is what the user picks in the wizard and submits to Run.
type Plan struct {
	SourcePath   string            `json:"sourcePath"`   // compose dir on source
	TargetPath   string            `json:"targetPath"`   // compose dir on target
	PostgresMode map[string]string `json:"postgresMode"` // service name → "volume" | "dump"
}

// Inventory is what Inspect returns: everything the wizard shows the user.
type Inventory struct {
	ProjectName string    `json:"projectName"`
	Services    []Service `json:"services"`
	Volumes     []Volume  `json:"volumes"`
	BindMounts  []string  `json:"bindMounts"` // shown as warnings; not migrated in v1
	EnvFiles    []string  `json:"envFiles"`   // paths relative to compose dir
	// ExternalNetworks are docker networks the compose file declares as external.
	// Compose won't create these on `up` — they must already exist — so the
	// migration recreates them on the target by name before starting the stack.
	ExternalNetworks []string `json:"externalNetworks"`
	// BuildImages are the resolved image tags of services that build from source.
	// The migration ships these prebuilt (docker save/load) so the target doesn't
	// have to reproduce the build context or run a builder.
	BuildImages []string `json:"buildImages"`
	Warnings    []string `json:"warnings"`
}

type Service struct {
	Name       string `json:"name"`
	Image      string `json:"image"`
	IsPostgres bool   `json:"isPostgres"`
	Builds     bool   `json:"builds"` // has a build: section — its image is shipped prebuilt
}

type Volume struct {
	Name     string `json:"name"`     // full docker volume name (project_volume)
	Size     int64  `json:"size"`     // bytes, best-effort
	Mountpoint string `json:"mountpoint"`
}
