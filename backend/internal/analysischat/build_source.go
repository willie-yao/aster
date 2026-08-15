package analysischat

import (
	"strings"

	"github.com/willie-yao/aster/backend/internal/buildsource"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

func resolveBuildSourceRepository(build models.BuildInfo, configured sourceinvestigation.Repository) (sourceinvestigation.Repository, bool) {
	source, ok := buildsource.Resolve(build, configured.Owner, configured.Name)
	if !ok {
		return sourceinvestigation.Repository{}, false
	}
	return sourceinvestigation.Repository{Owner: source.Owner, Name: source.Name, Revision: source.Revision}, true
}

func persistedBuildSourceRepository(
	resolved persistedResolvedAnalysis,
	configured sourceinvestigation.Repository,
) (sourceinvestigation.Repository, bool) {
	source, ok := resolveBuildSourceRepository(resolved.Build, configured)
	if !ok || sourceinvestigation.ValidateRepository(resolved.Source) != nil ||
		!strings.EqualFold(resolved.Source.Owner, source.Owner) ||
		!strings.EqualFold(resolved.Source.Name, source.Name) ||
		!strings.EqualFold(resolved.Source.Revision, source.Revision) {
		return sourceinvestigation.Repository{}, false
	}
	return source, true
}
