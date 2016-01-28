package conveyor

import "github.com/jmoiron/sqlx"

// Artifact represents an image that was successfully created from a build.
type Artifact struct {
	// Unique identifier for this artifact.
	ID string `db:"id"`
	// The build that this artifact was a result of.
	BuildID string `db:"build_id"`
	// The name of the image that was produced.
	Image string `db:"image"`
}

// artifactsCreate creates a new artifact linked to the build.
func artifactsCreate(tx *sqlx.Tx, a *Artifact) error {
	const createArtifactSql = `INSERT INTO artifacts (build_id, image) VALUES (:build_id, :image) RETURNING id`
	return insert(tx, createArtifactSql, a, &a.ID)
}
