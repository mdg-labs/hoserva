package template

import "crypto/ed25519"

// CatalogPublicKey is the Ed25519 public key that signs the curated
// catalog archive (Q65): the public half of the catalog repository's
// HOSERVA_CATALOG_SIGNING_KEY, published as signing-key.pub.pem at the root
// of mdg-labs/hoserva-catalog. It is not the release-signing key
// (internal/update.EmbeddedPublicKey): each compiled-in key verifies only
// its own kind of file (doc 01 §7), so this package never refers to that
// one. Tests inject another key on CatalogStore.Key.
var CatalogPublicKey = ed25519.PublicKey{
	0xdd, 0xfb, 0x9a, 0xd6, 0xa1, 0xf0, 0xdb, 0x76,
	0xbd, 0x50, 0xa0, 0xb2, 0x34, 0xb9, 0x0c, 0xeb,
	0x06, 0x79, 0xbc, 0xf8, 0x2b, 0x55, 0x62, 0xb5,
	0xc9, 0x59, 0x8a, 0xf3, 0xab, 0x87, 0x0f, 0x4f,
}
