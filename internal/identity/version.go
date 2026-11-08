package identity

import (
	"errors"
)

// identityVersion is the shape of the registry this build writes.
//
// It exists because its absence was a defect: an earlier release moved a
// principal's grants to a new member without converting the old one, and the
// file loaded cleanly and described nobody — every account lost its dataset
// memberships and no administrator could sign in. Nothing on disk said which
// shape it was, so nothing could have noticed.
//
// The conversion that recovered such a file has been removed. Nothing has ever
// been deployed on a release that wrote one, so it converted nothing and would
// have gone on converting nothing — a mechanism kept for a consumer who does
// not exist, which is the shape this project has spent a week learning to
// recognise. Pre-releases carry no migration guarantee; see docs/releasing.md.
//
// The number stays, because the number is not the migration. A field that
// moves between releases is a thing a future build can branch on, and removing
// it would re-arm the gun rather than put it down.
// Version 5 retains operator Console identities, website bindings and the
// single-use code's persistence choice. Older readers must not discard them.
const identityVersion = 5

// readable reports that a registry on disk is one this build can read without
// losing what it cannot name.
//
// Only the forward direction is checked. A registry from an older shape is
// read as it is, because no such registry exists to convert; a registry from a
// newer one is refused, because a build that cannot name a member cannot
// preserve it and would drop it on the next save — which is exactly how the
// defect above happened, in the other direction.
func (a *Authority) readable() error {
	if a.data.Version > identityVersion {
		return errors.New("identity store was written by a newer release and this one would discard what it cannot read")
	}
	return nil
}
