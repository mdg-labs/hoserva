package backup

import "strings"

// databaseImages are the image names, as the last path segment of an image
// repository, whose containers keep a database open in their appdata: a
// copy taken while one runs can be inconsistent (doc 10 §2).
var databaseImages = map[string]bool{
	"postgres":      true,
	"postgresql":    true,
	"postgis":       true,
	"pgvector":      true,
	"timescaledb":   true,
	"mariadb":       true,
	"mysql":         true,
	"percona":       true,
	"mongo":         true,
	"mongodb":       true,
	"redis":         true,
	"valkey":        true,
	"keydb":         true,
	"dragonfly":     true,
	"influxdb":      true,
	"couchdb":       true,
	"cassandra":     true,
	"clickhouse":    true,
	"cockroach":     true,
	"surrealdb":     true,
	"neo4j":         true,
	"arangodb":      true,
	"rethinkdb":     true,
	"elasticsearch": true,
	"opensearch":    true,
}

// IsDatabaseImage reports whether image, a repository with or without a
// registry, namespace, tag or digest, is a known database.
func IsDatabaseImage(image string) bool {
	name := strings.ToLower(strings.TrimSpace(image))
	if i := strings.Index(name, "@"); i >= 0 {
		name = name[:i]
	}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.Index(name, ":"); i >= 0 {
		name = name[:i]
	}
	return databaseImages[name]
}
