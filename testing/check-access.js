// Asks LibreChat itself whether the ACL rows this provider wrote actually grant what they
// were meant to, by calling the application's OWN permission code rather than reimplementing
// its queries or guessing at its HTTP API.
//
// Run it inside the running LibreChat container, which is where its node_modules and its
// compiled packages live:
//
//   docker compose -f testing/docker-compose.yml cp testing/check-access.js librechat:/tmp/
//   docker compose -f testing/docker-compose.yml exec librechat node /tmp/check-access.js
//
// Why this and not curl: LibreChat's REST API rejects a hand-made request with "Illegal
// request" before any permission check runs, so an HTTP probe cannot tell a working grant
// from a rejected request. These functions are the ones the API calls once it is satisfied.

const mongoose = require('/app/node_modules/mongoose');
const { createModels, createMethods } = require('/app/packages/data-schemas/dist/index.cjs');

const EXPECT = {
  agentId: 'agent_helpdesk',
  memberEmail: 'member@example.test',
  adminEmail: 'admin@example.test',
  outsiderEmail: 'outsider@example.test',
  groupName: 'Support',
};

let failures = 0;
function check(label, actual, expected) {
  const ok = actual === expected;
  if (!ok) failures++;
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${label}  (got ${actual}, want ${expected})`);
}

(async () => {
  await mongoose.connect(process.env.MONGO_URI);

  const models = createModels(mongoose);
  // createMethods wants the models registered on the connection the way the app registers
  // them, so that its internal lookups resolve.
  mongoose.models = Object.assign(mongoose.models || {}, models);
  const methods = createMethods(mongoose);

  const User = models.User;
  const Group = models.Group;
  const Agent = models.Agent;

  const admin = await User.findOne({ email: EXPECT.adminEmail }).lean();
  const member = await User.findOne({ email: EXPECT.memberEmail }).lean();
  const group = await Group.findOne({ name: EXPECT.groupName }).lean();
  const agent = await Agent.findOne({ id: EXPECT.agentId }).lean();

  if (!admin || !member || !agent || !group) {
    console.log('SETUP FAILED: apply examples/complete first.');
    console.log({ admin: !!admin, member: !!member, group: !!group, agent: !!agent });
    process.exit(1);
  }

  // The membership lookup LibreChat performs, spelled the way it spells it. This is the check
  // that catches memberIds having been written as ObjectIds instead of strings, which is
  // silent: the user simply resolves to no groups.
  const groups = await Group.find({
    memberIds: member.idOnTheSource || String(member._id),
  }).lean();
  check('the member resolves to their group', groups.length, 1);

  // getUserPrincipals is the application's own expansion of an account into everything it
  // counts as: itself, each group it belongs to, its role, and public. Going through it rather
  // than assembling the list by hand is the point - it is what makes the group grant below a
  // test of LibreChat's resolution instead of of my reading of it.
  const memberPrincipals = await methods.getUserPrincipals({
    userId: member._id,
    role: member.role,
  });
  check(
    'the group appears among the member\'s principals',
    memberPrincipals.some(
      (p) => p.principalType === 'group' && String(p.principalId) === String(group._id),
    ),
    true,
  );

  // VIEW = 1. getEffectivePermissions folds together every grant that applies to any of those
  // principals.
  const memberPerms = await methods.getEffectivePermissions(memberPrincipals, 'agent', agent._id);
  check('the group member can VIEW the agent', Boolean(memberPerms & 1), true);
  // EDIT = 2. A viewer grant must NOT carry it: EDIT is what LibreChat checks before allowing
  // a PATCH, and an edit made in the interface is drift the next apply silently overwrites.
  check('the group member cannot EDIT the agent', Boolean(memberPerms & 2), false);

  // The owner grant went to the ADMIN role, not to a person, so it has to apply to any admin.
  const adminPrincipals = await methods.getUserPrincipals({ userId: admin._id, role: admin.role });
  const adminPerms = await methods.getEffectivePermissions(adminPrincipals, 'agent', agent._id);
  check('an admin can VIEW the agent (role grant)', Boolean(adminPerms & 1), true);
  check('an admin can EDIT the agent (role grant)', Boolean(adminPerms & 2), true);
  check('an admin can DELETE the agent (role grant)', Boolean(adminPerms & 4), true);

  // A second admin created after the grant was made must inherit it, which is the whole
  // reason ownership goes to the role rather than to the author's account.
  const secondAdminPrincipals = await methods.getUserPrincipals({
    userId: new mongoose.Types.ObjectId(),
    role: 'ADMIN',
  });
  const secondAdminPerms = await methods.getEffectivePermissions(
    secondAdminPrincipals,
    'agent',
    agent._id,
  );
  check('an admin created later inherits the grant', Boolean(secondAdminPerms & 2), true);

  // And the negative case, which is the one that would make every positive result meaningless:
  // somebody in no group, holding the restricted role, must see nothing.
  const outsiderPrincipals = await methods.getUserPrincipals({
    userId: new mongoose.Types.ObjectId(),
    role: member.role,
  });
  const outsiderPerms = await methods.getEffectivePermissions(
    outsiderPrincipals,
    'agent',
    agent._id,
  );
  check('a non-member cannot VIEW the agent', Boolean(outsiderPerms & 1), false);

  // The MCP server is shared the same way, and it is worth checking separately: its ACL rows
  // use a different resourceType, so a viewer grant that worked for agents would still fail
  // here if accessroles were looked up with the wrong prefix.
  const mcp = await mongoose.connection.collection('mcpservers').findOne({ serverName: 'dummy' });
  const mcpPerms = await methods.getEffectivePermissions(memberPrincipals, 'mcpServer', mcp._id);
  check('the group member can VIEW the MCP server', Boolean(mcpPerms & 1), true);
  check('the group member cannot EDIT the MCP server', Boolean(mcpPerms & 2), false);

  // Role permissions are a different question from the ACL: whether the account may build an
  // agent of its own at all.
  const restrictedRole = await methods.getRoleByName(member.role);
  check('the restricted role may USE agents', restrictedRole.permissions.AGENTS.USE, true);
  check('the restricted role may NOT create agents', restrictedRole.permissions.AGENTS.CREATE, false);

  const userRole = await methods.getRoleByName('USER');
  check('the patched USER role may still USE agents', userRole.permissions.AGENTS.USE, true);
  check('the patched USER role may NOT create agents', userRole.permissions.AGENTS.CREATE, false);

  await mongoose.disconnect();
  console.log(failures === 0 ? '\nall checks passed' : `\n${failures} check(s) failed`);
  process.exit(failures === 0 ? 0 : 1);
})().catch((err) => {
  console.error(err);
  process.exit(1);
});
