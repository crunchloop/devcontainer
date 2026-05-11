package events

const (
	TypeBuildStart     = "build.start"
	TypeBuildLog       = "build.log"
	TypeBuildLayer     = "build.layer"
	TypeBuildCompleted = "build.completed"
)

// BuildSource identifies where the build originated.
type BuildSource string

const (
	BuildSourceImage      BuildSource = "image"
	BuildSourceDockerfile BuildSource = "dockerfile"
	BuildSourceCompose    BuildSource = "compose"
	BuildSourceFeatures   BuildSource = "features"
)

// BuildStartEvent fires once at the top of a build/pull operation.
type BuildStartEvent struct {
	Base
	Source BuildSource
	Ref    string // image ref being pulled, or build target tag
}

func (BuildStartEvent) EventType() string { return TypeBuildStart }

// BuildLogEvent carries a raw line of build/pull output. Stream is
// "stdout" or "stderr"; the docker build stream does not always
// distinguish, in which case Stream is "stdout".
type BuildLogEvent struct {
	Base
	Stream string
	Line   string
}

func (BuildLogEvent) EventType() string { return TypeBuildLog }

// BuildLayerEvent carries per-layer pull/build progress. Status values:
// "pulling", "extracting", "cached", "done".
type BuildLayerEvent struct {
	Base
	LayerID string
	Status  string
}

func (BuildLayerEvent) EventType() string { return TypeBuildLayer }

// BuildCompletedEvent fires when the build/pull completes successfully.
type BuildCompletedEvent struct {
	Base
	ImageID    string
	DurationMs int64
}

func (BuildCompletedEvent) EventType() string { return TypeBuildCompleted }
