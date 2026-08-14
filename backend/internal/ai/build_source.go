package ai

import (
	"github.com/willie-yao/aster/backend/internal/buildsource"
	"github.com/willie-yao/aster/backend/internal/models"
)

// BuildSource identifies one configured repository at the immutable commit tested by a build.
type BuildSource = buildsource.Source

// ResolveBuildSource fails closed when build metadata does not identify one exact source commit.
func ResolveBuildSource(build models.BuildInfo, owner, name string) (BuildSource, bool) {
	return buildsource.Resolve(build, owner, name)
}
