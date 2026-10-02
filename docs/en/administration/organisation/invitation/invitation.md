# Invitations

This page has not been translated yet. See the [French version](https://xolo-gateway.org/latest/) for the full content.

## Before upgrading invitations (INV-01)

Back up the database and stop writes while migrations run. Check duplicate
memberships on SQLite or PostgreSQL before deploying:

```sql
SELECT user_id, org_id, COUNT(*) AS membership_count
FROM memberships
GROUP BY user_id, org_id
HAVING COUNT(*) > 1;
```

Review every reported membership and its `membership_roles` rows. Migration
`202609300001` stops with an actionable diagnostic instead of merging or deleting
data. Resolve duplicates manually, then restart to install the unique
`(user_id, org_id)` index.

Migration `202609300002` revokes usable legacy XID invitation links. Administrators
can still list and delete them. **Recreate and redistribute every legacy link
that is still needed after upgrading.** Revocation is irreversible; restarting
does not reactivate links. New links use 256 random bits from `crypto/rand`,
encoded as unpadded URL-safe base64, with unchanged routes.

Targeted links are single-use and require an exact email match after sign-in.
Existing members retain their roles without consuming an invitation. Expiration
dates mean midnight UTC at the start of the chosen day. Only an empty use limit
means unlimited; other values must be positive integers.
