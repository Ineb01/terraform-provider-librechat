package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

// LibreChat's own default database name. Its MONGO_URI usually carries the name in the
// path (mongodb://mongodb:27017/LibreChat), so this only applies to a URI without one.
const defaultDatabase = "LibreChat"

// Collection names, as mongoose pluralises the model names in
// packages/data-schemas/src/models. They are lowercase and NOT snake_cased, which is why
// mcpservers and aclentries look the way they do - writing "mcpServers" creates a second,
// empty collection that LibreChat never reads, and the resource then appears to apply
// cleanly while nothing shows up in the interface.
const (
	collUsers       = "users"
	collGroups      = "groups"
	collAgents      = "agents"
	collMCPServers  = "mcpservers"
	collRoles       = "roles"
	collACLEntries  = "aclentries"
	collAccessRoles = "accessroles"
)

// Client is the provider's handle on LibreChat's database. There is no REST client here
// on purpose - see the package comment in main.go.
type Client struct {
	mongo *mongo.Client
	db    *mongo.Database
}

// NewClient connects and verifies the connection. Pinging here rather than lazily on the
// first resource means a wrong URI fails once, during Configure, with one clear message -
// instead of once per resource in the middle of an apply.
func NewClient(ctx context.Context, uri, database string) (*Client, error) {
	opts := options.Client().ApplyURI(uri).SetAppName("terraform-provider-librechat").
		// DefaultDocumentM makes a nested document decode into bson.M rather than the driver's
		// default bson.D. Both are correct BSON; the difference is not cosmetic here.
		//
		// LibreChat keeps two fields this provider reads as free-form documents - an MCP
		// server's `config` and an agent's `model_parameters`/`tool_options` - and anything
		// nested inside them arrives as an `any`. As a bson.D that is an ORDERED SLICE of
		// key/value pairs, so a type assertion to bson.M fails and json.Marshal renders it as
		// an array of {"Key":...,"Value":...} objects instead of an object.
		//
		// The visible symptom was a refresh that read an MCP server's `headers` back as absent,
		// which made every plan want to write them again - an apply that was never finished and
		// an Authorization header rewritten on each run.
		SetBSONOptions(&options.BSONOptions{DefaultDocumentM: true})

	mc, err := mongo.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("connecting to MongoDB: %w", err)
	}

	if err := mc.Ping(ctx, readpref.Primary()); err != nil {
		// Best effort: the connection is unusable anyway, and the ping error is the one
		// worth reporting.
		_ = mc.Disconnect(ctx)
		return nil, fmt.Errorf("pinging MongoDB: %w", err)
	}

	if database == "" {
		database = databaseFromURI(uri)
	}

	return &Client{mongo: mc, db: mc.Database(database)}, nil
}

// Disconnect exists for tests. There is deliberately no call to it in the provider
// itself: terraform-plugin-framework has no provider-level shutdown hook, and the plugin
// process is killed by the CLI at the end of the run, which closes the sockets anyway.
func (c *Client) Disconnect(ctx context.Context) error {
	return c.mongo.Disconnect(ctx)
}

// DatabaseName is reported in diagnostics, because "no such user" is a very different
// problem when the provider is pointed at the wrong database.
func (c *Client) DatabaseName() string { return c.db.Name() }

// databaseFromURI reads the database out of the connection string's path. Parsed with
// net/url rather than the driver's connstring package: that one lives under x/mongo/driver
// and is explicitly not part of the v2 public API.
func databaseFromURI(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return defaultDatabase
	}
	// The path may carry auth options after a ?, which url.Parse has already split off.
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" {
		return defaultDatabase
	}
	return name
}

func (c *Client) users() *mongo.Collection       { return c.db.Collection(collUsers) }
func (c *Client) groups() *mongo.Collection      { return c.db.Collection(collGroups) }
func (c *Client) agents() *mongo.Collection      { return c.db.Collection(collAgents) }
func (c *Client) mcpServers() *mongo.Collection  { return c.db.Collection(collMCPServers) }
func (c *Client) roles() *mongo.Collection       { return c.db.Collection(collRoles) }
func (c *Client) aclEntries() *mongo.Collection  { return c.db.Collection(collACLEntries) }
func (c *Client) accessRoles() *mongo.Collection { return c.db.Collection(collAccessRoles) }

// mongoUpsertReturningNew is the option set used wherever a write has to come back with the
// document it produced, so that the generated _id is available without a second round trip.
func mongoUpsertReturningNew() *options.FindOneAndUpdateOptionsBuilder {
	return options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)
}

// mongoReplaceUpsertReturningNew is the same for FindOneAndReplace, used by the resources
// that own a whole document rather than a set of fields.
func mongoReplaceUpsertReturningNew() *options.FindOneAndReplaceOptionsBuilder {
	return options.FindOneAndReplace().SetUpsert(true).SetReturnDocument(options.After)
}

// errGone marks "the document this resource tracks is no longer there", which every Read
// turns into a state removal rather than an error so a hand-deleted document is simply
// recreated on the next apply.
var errGone = errors.New("document not found")

// parseObjectID keeps the "that is not an ObjectId" complaint in one place. Every id this
// provider exposes is the 24-character hex form, so a failure here is nearly always a
// reference to the wrong attribute - an agent's agent_id ("agent_helpdesk") where its id was
// wanted, say.
func parseObjectID(hex string) (bson.ObjectID, error) {
	id, err := bson.ObjectIDFromHex(hex)
	if err != nil {
		return bson.NilObjectID, fmt.Errorf(
			"%q is not a MongoDB ObjectId (expected 24 hex characters): %w", hex, err)
	}
	return id, nil
}

// accessRoleDoc is the permission template an ACL grant points at. permBits is read from
// the database rather than hardcoded (agent_viewer = 1, agent_editor = 3, agent_owner = 15
// at the time of writing) so that a LibreChat release which adds a bit does not leave this
// provider writing a stale bitmask.
type accessRoleDoc struct {
	ID           bson.ObjectID `bson:"_id"`
	AccessRoleID string        `bson:"accessRoleId"`
	Name         string        `bson:"name"`
	Description  string        `bson:"description"`
	ResourceType string        `bson:"resourceType"`
	PermBits     int64         `bson:"permBits"`
}

// lookupAccessRole resolves e.g. ("agent", "viewer") -> the agent_viewer document.
// accessroles is seeded by LibreChat at startup, so a miss means either a typo in the role
// name or a LibreChat too old to use this ACL schema - both worth distinguishing from a
// generic query failure.
func (c *Client) lookupAccessRole(ctx context.Context, resourceType, roleName string) (*accessRoleDoc, error) {
	accessRoleID := resourceType + "_" + roleName

	var doc accessRoleDoc
	err := c.accessRoles().FindOne(ctx, bson.M{
		"resourceType": resourceType,
		"accessRoleId": accessRoleID,
	}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		known, kerr := c.knownAccessRoles(ctx, resourceType)
		if kerr != nil || len(known) == 0 {
			return nil, fmt.Errorf(
				"accessroles has no %q. LibreChat seeds that collection on startup, so an "+
					"empty one usually means this database has never had LibreChat running "+
					"against it", accessRoleID)
		}
		return nil, fmt.Errorf(
			"accessroles has no %q. Roles defined for resource_type %q: %s",
			accessRoleID, resourceType, strings.Join(known, ", "))
	}
	if err != nil {
		return nil, fmt.Errorf("reading accessroles: %w", err)
	}
	return &doc, nil
}

func (c *Client) knownAccessRoles(ctx context.Context, resourceType string) ([]string, error) {
	cur, err := c.accessRoles().Find(ctx, bson.M{"resourceType": resourceType})
	if err != nil {
		return nil, err
	}
	var docs []accessRoleDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(docs))
	for _, d := range docs {
		names = append(names, d.AccessRoleID)
	}
	return names, nil
}

// roleExists is used before writing a role name anywhere it matters - a user's role field,
// an ACL grant to principal_type "role". Mongo would accept an unknown name happily, and
// the result is an account with no permissions at all or a grant that matches nobody.
func (c *Client) roleExists(ctx context.Context, name string) (bool, error) {
	err := c.roles().FindOne(ctx, bson.M{"name": name}).Err()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading roles: %w", err)
	}
	return true, nil
}

func (c *Client) knownRoleNames(ctx context.Context) ([]string, error) {
	cur, err := c.roles().Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	var docs []struct {
		Name string `bson:"name"`
	}
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(docs))
	for _, d := range docs {
		names = append(names, d.Name)
	}
	return names, nil
}
