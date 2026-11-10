# Scoped record access

The proposed slice covers creator attribution, sharing a growing collection of
records, and revocable contribution rights within a collection. A collection
would be explicit storage membership in one dataset. Arbitrary field predicates
and application rules stay outside the authorization model.

1. **Creator attribution.** Let a schema declare a server-populated creator field.
   Test that record writers cannot supply or change it and that existing records
   are not attributed retroactively. Use the authenticated principal and a
   dataset-local pseudonym.
2. **Collection read grants.** Define an explicit collection identity, optionally
   anchored to a record, and how members enter and leave it. Read grants should cover newly admitted records,
   with the authorized view applied before lookup, query, count, aggregate,
   pagination and content resolution. Specify and test the effects of membership
   changes, revocation, expiry and existing frozen shares.
3. **Scoped contribution and revision.** Define a grant that authorizes insertion
   into one collection for selected principals or a deployment-defined
   authenticated audience. Allow a collection controller to grant and revoke
   contribution rights without conferring dataset write authority. Bind membership
   and creator at insertion. A separate
   right may permit a creator to edit or remove their own contributions within
   that scope. Validate current authority in the write transaction, including
   attachment operations and receipt recovery. Specify which single-record and
   bulk routes are admitted and test every refusal.
4. **Erasure and conformance.** Specify how an authorized deployment can enumerate
   and erase a principal's contributions in each dataset it administers. The host
   coordinates account-wide cleanup across datasets and owns any placeholder it
   chooses to retain. Verify concealment, repeatable cleanup, content access and
   expiry on both SQL engines, including restore from a pre-feature backup.

Only creator attribution is implemented in this increment. The remaining stages
are proposals. Define each contract and its conformance cases before advertising
its capability. Keep existing grants and their behavior independent of the new
rights.
