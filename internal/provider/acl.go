package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Principal types, as LibreChat's PrincipalType enum spells them. Each is identified
// differently, and getting that wrong is silent: the grant is written, LibreChat's lookup
// matches nothing, and the resource simply never appears for the people it was shared with.
//
//	public - no principalId at all
//	role   - the role NAME as principalId ("ADMIN"), not the role document's _id
//	group  - the group's _id as an ObjectId
//	user   - the user's _id as an ObjectId
const (
	principalTypeUser   = "user"
	principalTypeGroup  = "group"
	principalTypeRole   = "role"
	principalTypePublic = "public"
)

// principalModel is the refPath value that goes with each principal type.
const (
	principalModelUser  = "User"
	principalModelGroup = "Group"
	principalModelRole  = "Role"
)

// Resource types that can carry an ACL, from the accessRole schema's enum.
var validResourceTypes = []string{
	"agent", "project", "file", "promptGroup", "mcpServer", "remoteAgent", "skill", "sharedLink",
}

// principal is the resolved, ready-to-store identity of an ACL grantee.
type principal struct {
	Type  string
	ID    any    // bson.ObjectID, string, or nil for public
	Model string // "" for public
}

// filter contributes the principal's half of a query that identifies one grant. principalId
// is omitted entirely for a public grant, matching how the document is stored - a filter of
// {principalId: null} would not match a document with no such field under every MongoDB
// version.
func (p principal) filter() bson.M {
	f := bson.M{"principalType": p.Type}
	if p.ID != nil {
		f["principalId"] = p.ID
	}
	return f
}

func (p principal) fields() bson.M {
	f := bson.M{"principalType": p.Type}
	if p.ID != nil {
		f["principalId"] = p.ID
		f["principalModel"] = p.Model
	}
	return f
}

// String is for diagnostics: "group/674f...", "role/ADMIN", "public".
func (p principal) String() string {
	if p.ID == nil {
		return p.Type
	}
	return fmt.Sprintf("%s/%v", p.Type, p.ID)
}

// resolvePrincipal turns the configured pair into a storable principal, verifying that the
// thing being granted to actually exists. Every check here guards against the same failure
// mode: Mongo accepts any value, so an unresolvable principal produces a grant that is
// present in the database, visible in state, and effective for nobody.
func (c *Client) resolvePrincipal(ctx context.Context, ptype, pid string) (principal, error) {
	switch ptype {
	case principalTypePublic:
		if pid != "" {
			return principal{}, errors.New(
				"principal_id must be omitted for principal_type \"public\": a public grant applies to everyone and names nobody")
		}
		return principal{Type: principalTypePublic}, nil

	case principalTypeRole:
		if pid == "" {
			return principal{}, errors.New("principal_id is required for principal_type \"role\": it is the role's name, e.g. \"ADMIN\"")
		}
		exists, err := c.roleExists(ctx, pid)
		if err != nil {
			return principal{}, err
		}
		if !exists {
			known, _ := c.knownRoleNames(ctx)
			return principal{}, fmt.Errorf(
				"the roles collection has no %q. Known roles: %s. Note that a role principal is "+
					"identified by NAME, not by ObjectId", pid, strings.Join(known, ", "))
		}
		return principal{Type: principalTypeRole, ID: pid, Model: principalModelRole}, nil

	case principalTypeGroup:
		id, err := requireExistingObjectID(ctx, c.groups(), pid, "group")
		if err != nil {
			return principal{}, err
		}
		return principal{Type: principalTypeGroup, ID: id, Model: principalModelGroup}, nil

	case principalTypeUser:
		id, err := requireExistingObjectID(ctx, c.users(), pid, "user")
		if err != nil {
			return principal{}, err
		}
		return principal{Type: principalTypeUser, ID: id, Model: principalModelUser}, nil

	default:
		return principal{}, fmt.Errorf(
			"unknown principal_type %q; expected one of %s, %s, %s, %s",
			ptype, principalTypeUser, principalTypeGroup, principalTypeRole, principalTypePublic)
	}
}

func requireExistingObjectID(ctx context.Context, coll *mongo.Collection, hex, what string) (bson.ObjectID, error) {
	if hex == "" {
		return bson.NilObjectID, fmt.Errorf("principal_id is required for principal_type %q", what)
	}
	id, err := parseObjectID(hex)
	if err != nil {
		return bson.NilObjectID, err
	}
	err = coll.FindOne(ctx, bson.M{"_id": id}).Err()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return bson.NilObjectID, fmt.Errorf("no %s with id %s exists", what, hex)
	}
	if err != nil {
		return bson.NilObjectID, err
	}
	return id, nil
}

// aclEntryDoc is one row of the aclentries collection.
type aclEntryDoc struct {
	ID             bson.ObjectID  `bson:"_id"`
	PrincipalType  string         `bson:"principalType"`
	PrincipalID    any            `bson:"principalId"`
	PrincipalModel string         `bson:"principalModel"`
	ResourceType   string         `bson:"resourceType"`
	ResourceID     bson.ObjectID  `bson:"resourceId"`
	PermBits       int64          `bson:"permBits"`
	RoleID         *bson.ObjectID `bson:"roleId"`
	GrantedBy      *bson.ObjectID `bson:"grantedBy"`
}

// principalIDString renders a stored principalId back into the form the configuration uses:
// hex for the ObjectId types, the name itself for a role.
func (d aclEntryDoc) principalIDString() string {
	switch v := d.PrincipalID.(type) {
	case nil:
		return ""
	case bson.ObjectID:
		return v.Hex()
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

// deleteGrantsForResource removes every ACL row attached to a resource. Used when a resource
// this provider owns is destroyed: LibreChat has no cascade, and rows pointing at a deleted
// agent would otherwise stay forever, still granting access to an id that will never be
// reused but is impossible to audit.
func (c *Client) deleteGrantsForResource(ctx context.Context, resourceType string, resourceID bson.ObjectID) error {
	_, err := c.aclEntries().DeleteMany(ctx, bson.M{
		"resourceType": resourceType,
		"resourceId":   resourceID,
	})
	return err
}
