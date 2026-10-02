package recoverykit

import (
	"fmt"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/settings"
)

// readme returns the recovery steps of kit. The decryption step follows the key
// material the kit carries: identities.txt when the server holds private keys, the
// administrator's own key file in recipient-only mode, the backup passphrase in
// passphrase mode.
func readme(kit *Kit) string {
	m := kit.manifest
	hasIdentities := kit.identities != ""
	recipientOnly := !hasIdentities && len(m.Encryption.Recipients) > 0
	passphrase := m.Encryption.Mode == settings.ModePassphrase || m.Encryption.PassphraseConfigured || m.Encryption.RetiredPassphrases > 0
	prefix := m.MetadataPrefix
	if prefix == "" {
		prefix = "_mongorescue/metadata/<install_id>/"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "MongoRescue recovery kit\n========================\n\nCreated %s", m.CreatedAt.Format(time.RFC3339))
	if m.Version != "" {
		fmt.Fprintf(&b, " by MongoRescue %s", m.Version)
	}
	b.WriteString(".\n\n")
	b.WriteString(`This kit holds secrets in plain form. Keep it offline (a password manager, an
encrypted USB stick, a safe), separate from your backups, and never commit or
mail it. Download a new kit whenever MongoRescue reminds you to.

Contents
--------
secret.key      The key that seals the credentials stored in mongorescue.db and in
                its metadata snapshots. A database copy cannot be used without it.
recovery.json   Encryption settings, every storage target with its credentials and
                the location of the latest metadata snapshot.
`)
	if hasIdentities {
		b.WriteString(`identities.txt  The age private keys (current and retired) that decrypt encrypted
                backups and metadata snapshots.
`)
	}
	b.WriteString("Passphrases and private keys this server does not hold are never part of the kit.\n\n")

	b.WriteString("Restore MongoRescue from a metadata snapshot\n--------------------------------------------\n")
	b.WriteString("1. Install the same or a newer MongoRescue release on the new host. Do not start it.\n")
	fmt.Fprintf(&b, `2. Download the latest snapshot of this installation from the storage target named
   in recovery.json (metadata_snapshot: target and key), under the prefix
     %s
   using the target's credentials in recovery.json. Other installations sharing
   the bucket write under their own prefix.
`, prefix)
	b.WriteString("3. Decrypt it with age:\n")
	switch {
	case hasIdentities:
		b.WriteString("     age -d -i identities.txt -o mongorescue.db mongorescue-<time>.db.age\n")
	case recipientOnly:
		b.WriteString(`   This server holds no private key (recipient-only mode), so the kit has none.
   Use the private key you keep for the recipients listed in recovery.json:
     age -d -i <your key file> -o mongorescue.db mongorescue-<time>.db.age
`)
	}
	if passphrase {
		b.WriteString("   With passphrase encryption, run age -d -o mongorescue.db <file> and enter the\n   backup passphrase (it is not in the kit).\n")
	}
	b.WriteString(`   A snapshot ending in .db is not encrypted: rename it.
4. Put mongorescue.db and secret.key into the data directory and restrict them:
     chmod 600 mongorescue.db secret.key
   If the old installation used MONGORESCUE_SECRET_KEY, set it to the content of
   secret.key instead of copying the file.
5. Start MongoRescue. Jobs, backup records, users, settings and storage targets are
   back; sign in with your usual account.

Without a snapshot
------------------
Start a fresh installation with this secret.key, recreate the storage targets from
recovery.json, `)
	switch {
	case hasIdentities:
		b.WriteString("put the private keys from identities.txt")
	case recipientOnly:
		b.WriteString("add the recipients from recovery.json (and your own private key, to restore)")
	default:
		b.WriteString("configure the backup encryption")
	}
	if passphrase {
		b.WriteString(" or your passphrase")
	}
	b.WriteString(` under
Settings > Encryption, and import the archives found by a storage scan (Settings >
Storage > Scan now, then Import).
`)
	if m.MetadataSnapshot == nil {
		b.WriteString("\nNote: no metadata snapshot had been stored when this kit was created. Turn on\nSettings > Recovery > Metadata backups.\n")
	}
	return b.String()
}
