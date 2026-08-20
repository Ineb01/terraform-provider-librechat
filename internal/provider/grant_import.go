package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// An ACL row has no name of its own, which used to make it the one resource here that could
// only be imported by ObjectId. That is a poor trade for the case it matters in: adopting an
// estate somebody else built means importing dozens of grants at once, and every id has to be
// dug out of MongoDB first.
//
// So a grant can also be named by what actually identifies it - the resource and the principal:
//
//	agent/agent_dev_iqs/group/sap
//	mcpServer/kb-iqs/role/ADMIN
//	agent/agent_dev_iqs/public
//
// The tuple (resourceType, resourceId, principalType, principalId) is what LibreChat itself
// indexes and treats as one grant, so it identifies a single row. It is not declared UNIQUE in
// the collection, though, and a multi-tenant deployment separates rows by tenantId - hence the
// ambiguity check in ImportState rather than a blind first match.
const grantImportGrammar = "resource_type/resource_key/principal_type[/principal_id]"

// grantImportID is the parsed form. Nothing here has been checked against the database yet.
type grantImportID struct {
	ResourceType  string
	ResourceKey   string
	PrincipalType string
	PrincipalID   string // empty for a public grant
}

// parseGrantImportID splits and validates an import id. It does no lookups, so it is a pure
// function and the tests do not need a database.
func parseGrantImportID(raw string) (grantImportID, error) {
	// SplitN with 4 so the LAST field may contain a slash. A principal id can be an email or
	// a group name; the three fields before it cannot legitimately contain one.
	parts := strings.SplitN(strings.TrimSpace(raw), "/", 4)
	if len(parts) < 3 {
		return grantImportID{}, fmt.Errorf(
			"%q is neither an ObjectId nor %s", raw, grantImportGrammar)
	}

	out := grantImportID{
		ResourceType:  strings.TrimSpace(parts[0]),
		ResourceKey:   strings.TrimSpace(parts[1]),
		PrincipalType: strings.TrimSpace(parts[2]),
	}
	if len(parts) == 4 {
		out.PrincipalID = strings.TrimSpace(parts[3])
	}

	if out.ResourceKey == "" {
		return grantImportID{}, errors.New(
			"the resource is missing: give an agent id, an MCP server name, or the resource's ObjectId")
	}

	if !contains(validResourceTypes, out.ResourceType) {
		return grantImportID{}, fmt.Errorf(
			"unknown resource type %q; expected one of %s",
			out.ResourceType, strings.Join(validResourceTypes, ", "))
	}

	switch out.PrincipalType {
	case principalTypePublic:
		if out.PrincipalID != "" {
			return grantImportID{}, fmt.Errorf(
				"a public grant names nobody, so drop the trailing %q: %s/%s/public",
				out.PrincipalID, out.ResourceType, out.ResourceKey)
		}
	case principalTypeUser, principalTypeGroup, principalTypeRole:
		if out.PrincipalID == "" {
			return grantImportID{}, fmt.Errorf(
				"principal type %q needs a principal: a %s", out.PrincipalType,
				map[string]string{
					principalTypeUser:  "user's email or ObjectId",
					principalTypeGroup: "group's name or ObjectId",
					principalTypeRole:  "role name, e.g. ADMIN",
				}[out.PrincipalType])
		}
	default:
		return grantImportID{}, fmt.Errorf(
			"unknown principal type %q; expected one of %s, %s, %s, %s", out.PrincipalType,
			principalTypeUser, principalTypeGroup, principalTypeRole, principalTypePublic)
	}

	return out, nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// resolveImportResourceID turns the resource half of an import id into an ObjectId.
//
// Only the two resource types this provider manages have a natural key to look up. For the
// rest the ObjectId is the answer, and saying so is better than a lookup that silently finds
// nothing: this provider does not know how a sharedLink or a file is named.
func (r *grantResource) resolveImportResourceID(ctx context.Context, resourceType, key string) (bson.ObjectID, error) {
	if id, err := bson.ObjectIDFromHex(key); err == nil {
		return id, nil
	}

	switch resourceType {
	case "agent":
		// The agent's own "id" field - agent_dev_iqs - not the document's _id.
		return lookupObjectIDByField(ctx, r.client.agents(), "id", key, "agent")
	case "mcpServer":
		return lookupObjectIDByField(ctx, r.client.mcpServers(), "serverName", key, "MCP server")
	default:
		return bson.ObjectID{}, fmt.Errorf(
			"%q is not an ObjectId, and this provider cannot look a %s up by name - "+
				"only agent (by agent id) and mcpServer (by server name) have a natural key here. "+
				"Use the resource's ObjectId", key, resourceType)
	}
}

// resolveImportPrincipal accepts the names a person has to hand - a group's name, a user's
// email - as well as the ObjectIds a configuration uses.
//
// resolvePrincipal stays stricter on purpose: in a configuration the id comes from a resource
// reference, so a name there is a mistake worth reporting rather than resolving.
func (r *grantResource) resolveImportPrincipal(ctx context.Context, ptype, pid string) (principal, error) {
	switch ptype {
	case principalTypeGroup:
		if _, err := bson.ObjectIDFromHex(pid); err != nil {
			id, lookupErr := lookupObjectIDByField(ctx, r.client.groups(), "name", pid, "group")
			if lookupErr != nil {
				return principal{}, lookupErr
			}
			return principal{Type: principalTypeGroup, ID: id, Model: principalModelGroup}, nil
		}
	case principalTypeUser:
		if _, err := bson.ObjectIDFromHex(pid); err != nil {
			// Emails are stored lowercase - the user resource enforces it - so an import id
			// typed with capitals should still find the account.
			id, lookupErr := lookupObjectIDByField(ctx, r.client.users(), "email",
				strings.ToLower(pid), "user")
			if lookupErr != nil {
				return principal{}, lookupErr
			}
			return principal{Type: principalTypeUser, ID: id, Model: principalModelUser}, nil
		}
	}

	return r.client.resolvePrincipal(ctx, ptype, pid)
}

func lookupObjectIDByField(ctx context.Context, coll *mongo.Collection, field, value, what string) (bson.ObjectID, error) {
	var doc struct {
		ID bson.ObjectID `bson:"_id"`
	}
	err := coll.FindOne(ctx, bson.M{field: value}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return bson.ObjectID{}, fmt.Errorf("no %s has %s %q", what, field, value)
	}
	if err != nil {
		return bson.ObjectID{}, err
	}
	return doc.ID, nil
}
