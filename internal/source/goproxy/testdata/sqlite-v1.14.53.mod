// Deprecated: use example.test/db/sqlite/v3 instead.
module example.test/db/sqlite

go 1.19

retract (
	[v2.0.0+incompatible, v2.0.3+incompatible] // Accidental tags, never a release.
	v1.14.50 // Broken "build"
	v1.14.51
)

retract "v1.14.49" // quoted string
