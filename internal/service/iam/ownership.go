package iam

import (
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
)

// The access-key ownership rule shared by every IAM implementation: root
// manages every key, a user only the keys it owns, derived credentials
// (service accounts, STS sessions) and anonymous callers none. Derived
// credentials are excluded so that a restricted key cannot mint a broader
// one for its parent.

func errNotOwner(detail string) error {
	return fmt.Errorf("%w: iam: %s", domain.ErrAccessDenied, detail)
}

// checkKeyManager rejects the callers that may not manage access keys at
// all. Derived credentials are checked first: the service accounts and STS
// sessions of root carry IsRoot too.
func checkKeyManager(caller domain.Identity) error {
	switch {
	case caller.Parent != "":
		return errNotOwner("derived credentials cannot manage access keys")
	case caller.IsRoot:
		return nil
	case caller.IsAnonymous || caller.AccessKey == "":
		return errNotOwner("anonymous callers cannot manage access keys")
	}
	return nil
}

// keyParent resolves the user an access-key creation or listing applies to.
// Root may name any user; an empty name means root itself on creation and
// every user on listing. Any other caller may only name itself.
func keyParent(caller domain.Identity, requested string, listing bool) (string, error) {
	if err := checkKeyManager(caller); err != nil {
		return "", err
	}
	if caller.IsRoot {
		if requested == "" && !listing {
			return caller.AccessKey, nil
		}
		return requested, nil
	}
	if requested != "" && requested != caller.AccessKey {
		return "", errNotOwner("only root may manage the access keys of other users")
	}
	return caller.AccessKey, nil
}

// checkKeyOwner verifies that caller may read or change a key owned by parent.
func checkKeyOwner(caller domain.Identity, parent string) error {
	if err := checkKeyManager(caller); err != nil {
		return err
	}
	if caller.IsRoot || parent == caller.AccessKey {
		return nil
	}
	return errNotOwner("only root may manage the access keys of other users")
}
