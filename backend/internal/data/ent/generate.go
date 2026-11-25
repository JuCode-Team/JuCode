package ent

// Ent code generator. Run `go generate ./internal/data/ent` after any
// edit under ./schema. We pin ent via go.mod so `-mod=mod` will fetch a
// matching CLI build without a separate `go install`.
//
//go:generate go run -mod=mod entgo.io/ent/cmd/ent generate --feature sql/upsert,sql/lock ./schema
