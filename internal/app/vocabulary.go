package app

import (
	"context"

	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/reference"
)

// vocabulary adapts the reference data to the small ports the other modules
// validate against. Every call reads the store, so a row deactivated by an
// administrator stops being accepted immediately; nothing is cached here.
type vocabulary struct{ ref *reference.Service }

// RequestTypes implements propertyrequests.Vocabulary.
func (v vocabulary) RequestTypes(ctx context.Context) ([]string, error) {
	return v.ref.ActiveCodes(ctx, reference.SetRequestTypes)
}

// ContactMethods implements propertyrequests.Vocabulary and businessenquiries.Vocabulary.
func (v vocabulary) ContactMethods(ctx context.Context) ([]string, error) {
	return v.ref.ActiveCodes(ctx, reference.SetContactMethods)
}

// DirectoryKinds implements businessenquiries.Vocabulary.
func (v vocabulary) DirectoryKinds(ctx context.Context) ([]string, error) {
	return v.ref.ActiveCodes(ctx, reference.SetDirectoryKinds)
}

// SelectableRoles implements identity.RoleCatalogue. Platform roles are
// refused by identity itself before this list is consulted.
func (v vocabulary) SelectableRoles(ctx context.Context) ([]identity.SelectableRole, error) {
	items, err := v.ref.Items(ctx, reference.SetAccountRoles)
	if err != nil {
		return nil, err
	}
	out := make([]identity.SelectableRole, 0, len(items))
	for _, it := range items {
		// A role whose verification flag is missing is treated as needing verification.
		out = append(out, identity.SelectableRole{Code: it.Code, RequiresVerification: it.RequiresVerification == nil || *it.RequiresVerification})
	}
	return out, nil
}

// DirectoryKindList implements directory.Reference: the active directory
// kinds in display order.
func (v vocabulary) DirectoryKindList(ctx context.Context) ([]directory.Kind, error) {
	items, err := v.ref.Items(ctx, reference.SetDirectoryKinds)
	if err != nil {
		return nil, err
	}
	out := make([]directory.Kind, 0, len(items))
	for _, it := range items {
		k := directory.Kind{Code: it.Code, Label: it.Label, SingularLabel: it.Label}
		if it.SingularLabel != nil {
			k.SingularLabel = *it.SingularLabel
		}
		if it.Description != nil {
			k.Description = *it.Description
		}
		out = append(out, k)
	}
	return out, nil
}

// Labels implements directory.Reference.
func (v vocabulary) Labels(ctx context.Context, set string) (map[string]string, error) {
	return v.ref.Labels(ctx, set)
}
