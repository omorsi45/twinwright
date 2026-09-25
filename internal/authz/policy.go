package authz

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	"twinwright/internal/compiler"
)

// permissions is the complete allowlist. A behavior missing here has no
// permission, so a secured run denies it.
var permissions = map[string]string{
	"billing.getCustomer":     "customers.read",
	"billing.listInvoices":    "invoices.read",
	"billing.listCharges":     "charges.read",
	"billing.getCharge":       "charges.read",
	"billing.createRefund":    "refunds.create",
	"billing.getSubscription": "subscriptions.read",
	"crm.getAccount":          "crm.accounts.read",
	"crm.searchAccounts":      "crm.accounts.read",
	"crm.addAccountNote":      "crm.notes.write",
	"crm.updateAccountStatus": "crm.accounts.write",
	"ticket.createIssue":      "jira.issues.create",
	"ticket.getIssue":         "jira.issues.read",
	"ticket.searchIssues":     "jira.issues.read",
	"ticket.addComment":       "jira.comments.write",
	"ticket.transitionIssue":  "jira.issues.transition",
	"messaging.listChannels":  "slack.channels.read",
	"messaging.readChannel":   "slack.messages.read",
	"messaging.postMessage":   "slack.messages.write",
}

// Permission returns the permission that authorizes an operation's behavior.
func Permission(op compiler.Operation) (string, bool) {
	permission, ok := permissions[op.Behavior]
	return permission, ok
}

// Policy is the canonical principal policy stored with a run.
type Policy struct {
	Version         int              `json:"version"`
	Principal       Principal        `json:"principal"`
	Roles           map[string]Role  `json:"roles,omitempty"`
	Permissions     Role             `json:"permissions"`
	Resources       Resources        `json:"resources"`
	Constraints     Constraints      `json:"constraints"`
	TemporaryGrants []TemporaryGrant `json:"temporary_grants,omitempty"`
	Revocations     []Revocation     `json:"revocations,omitempty"`
}

type Principal struct {
	ID    string   `json:"id"`
	Roles []string `json:"roles,omitempty"`
}

type Role struct {
	Allow []string `json:"allow,omitempty"`
}

// Resources scopes are optional: nil imposes no scope, an empty list denies
// every resource of that kind.
type Resources struct {
	CustomerIDs *[]string `json:"customer_ids,omitempty"`
	ChannelIDs  *[]string `json:"channel_ids,omitempty"`
	ProjectIDs  *[]string `json:"project_ids,omitempty"`
}

type Constraints struct {
	RefundMaxCents *int `json:"refund_max_cents,omitempty"`
}

// TemporaryGrant is active for the inclusive range of run-local call numbers.
type TemporaryGrant struct {
	Permission   string `json:"permission"`
	StartsAtCall int    `json:"starts_at_call"`
	EndsAtCall   int    `json:"ends_at_call"`
}

// Revocation removes a permission starting at call AfterCall+1.
type Revocation struct {
	Permission string `json:"permission"`
	AfterCall  int    `json:"after_call"`
}

type rawPolicy struct {
	Version   int `yaml:"version"`
	Principal struct {
		ID    string   `yaml:"id"`
		Roles []string `yaml:"roles"`
	} `yaml:"principal"`
	Roles map[string]struct {
		Allow []string `yaml:"allow"`
	} `yaml:"roles"`
	Permissions struct {
		Allow []string `yaml:"allow"`
	} `yaml:"permissions"`
	Resources struct {
		CustomerIDs *[]string `yaml:"customer_ids"`
		ChannelIDs  *[]string `yaml:"channel_ids"`
		ProjectIDs  *[]string `yaml:"project_ids"`
	} `yaml:"resources"`
	Constraints struct {
		RefundMaxCents *int `yaml:"refund_max_cents"`
	} `yaml:"constraints"`
	TemporaryGrants []struct {
		Permission   string `yaml:"permission"`
		StartsAtCall *int   `yaml:"starts_at_call"`
		EndsAtCall   *int   `yaml:"ends_at_call"`
	} `yaml:"temporary_grants"`
	Revocations []struct {
		Permission string `yaml:"permission"`
		AfterCall  *int   `yaml:"after_call"`
	} `yaml:"revocations"`
}

var name = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*$`)

// Parse rejects any policy that is ambiguous or grants a permission the world
// cannot use, before a world or run is created.
func Parse(raw []byte, manifest compiler.Manifest) (Policy, error) {
	if err := compiler.ValidateManifest(manifest); err != nil {
		return Policy{}, fmt.Errorf("manifest: %w", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return Policy{}, fmt.Errorf("authorization policy YAML: %w", err)
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return Policy{}, fmt.Errorf("authorization policy must be a mapping")
	}
	for i := 0; i < len(node.Content[0].Content); i += 2 {
		if node.Content[0].Content[i].Value == "version" && node.Content[0].Content[i+1].Tag != "!!int" {
			return Policy{}, fmt.Errorf("authorization policy version must be an integer")
		}
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var input rawPolicy
	if err := decoder.Decode(&input); err != nil {
		return Policy{}, fmt.Errorf("authorization policy YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Policy{}, fmt.Errorf("authorization policy YAML: %w", err)
		}
		return Policy{}, fmt.Errorf("authorization policy has multiple YAML documents")
	}
	if input.Version != 1 {
		return Policy{}, fmt.Errorf("unsupported authorization policy version %d", input.Version)
	}
	if !name.MatchString(input.Principal.ID) {
		return Policy{}, fmt.Errorf("invalid principal ID %q", input.Principal.ID)
	}
	available := map[string]bool{}
	for _, op := range manifest.Operations {
		if permission, ok := Permission(op); ok {
			available[permission] = true
		}
	}
	checkPermission := func(permission string) error {
		if !known(permission) {
			return fmt.Errorf("unknown permission %q", permission)
		}
		if !available[permission] {
			return fmt.Errorf("permission %s has no operation in this world", permission)
		}
		return nil
	}
	allowList := func(context string, list []string) ([]string, error) {
		for _, permission := range list {
			if err := checkPermission(permission); err != nil {
				return nil, fmt.Errorf("%s: %w", context, err)
			}
		}
		return uniqueSorted(context+" allow", list)
	}

	policy := Policy{Version: 1, Principal: Principal{ID: input.Principal.ID}}
	if len(input.Roles) > 0 {
		policy.Roles = map[string]Role{}
	}
	for roleName, role := range input.Roles {
		if !name.MatchString(roleName) {
			return Policy{}, fmt.Errorf("invalid role name %q", roleName)
		}
		allow, err := allowList("role "+roleName, role.Allow)
		if err != nil {
			return Policy{}, err
		}
		policy.Roles[roleName] = Role{Allow: allow}
	}
	for _, roleName := range input.Principal.Roles {
		if _, ok := policy.Roles[roleName]; !ok {
			return Policy{}, fmt.Errorf("principal role %q is not defined", roleName)
		}
	}
	roles, err := uniqueSorted("principal role", input.Principal.Roles)
	if err != nil {
		return Policy{}, err
	}
	policy.Principal.Roles = roles
	if policy.Permissions.Allow, err = allowList("permissions", input.Permissions.Allow); err != nil {
		return Policy{}, err
	}

	for _, scope := range []struct {
		kind string
		in   *[]string
		out  **[]string
	}{
		{"customer_ids", input.Resources.CustomerIDs, &policy.Resources.CustomerIDs},
		{"channel_ids", input.Resources.ChannelIDs, &policy.Resources.ChannelIDs},
		{"project_ids", input.Resources.ProjectIDs, &policy.Resources.ProjectIDs},
	} {
		if scope.in == nil {
			continue
		}
		for _, id := range *scope.in {
			if id == "" || strings.TrimSpace(id) != id {
				return Policy{}, fmt.Errorf("invalid %s entry %q", scope.kind, id)
			}
		}
		ids, err := uniqueSorted(scope.kind, *scope.in)
		if err != nil {
			return Policy{}, err
		}
		if ids == nil {
			ids = []string{}
		}
		*scope.out = &ids
	}

	if limit := input.Constraints.RefundMaxCents; limit != nil {
		if *limit < 0 {
			return Policy{}, fmt.Errorf("refund_max_cents must be nonnegative")
		}
		policy.Constraints.RefundMaxCents = limit
	}

	for _, grant := range input.TemporaryGrants {
		if err := checkPermission(grant.Permission); err != nil {
			return Policy{}, fmt.Errorf("temporary grant: %w", err)
		}
		if grant.StartsAtCall == nil || grant.EndsAtCall == nil || *grant.StartsAtCall < 1 || *grant.EndsAtCall < *grant.StartsAtCall {
			return Policy{}, fmt.Errorf("temporary grant %s requires 1 <= starts_at_call <= ends_at_call", grant.Permission)
		}
		policy.TemporaryGrants = append(policy.TemporaryGrants, TemporaryGrant{Permission: grant.Permission, StartsAtCall: *grant.StartsAtCall, EndsAtCall: *grant.EndsAtCall})
	}
	sort.Slice(policy.TemporaryGrants, func(i, j int) bool {
		a, b := policy.TemporaryGrants[i], policy.TemporaryGrants[j]
		return a.Permission < b.Permission || a.Permission == b.Permission && a.StartsAtCall < b.StartsAtCall
	})
	for i := 1; i < len(policy.TemporaryGrants); i++ {
		prev, next := policy.TemporaryGrants[i-1], policy.TemporaryGrants[i]
		if prev.Permission == next.Permission && next.StartsAtCall <= prev.EndsAtCall {
			return Policy{}, fmt.Errorf("temporary grants for %s overlap", next.Permission)
		}
	}

	revoked := map[string]bool{}
	for _, revocation := range input.Revocations {
		if err := checkPermission(revocation.Permission); err != nil {
			return Policy{}, fmt.Errorf("revocation: %w", err)
		}
		if revocation.AfterCall == nil || *revocation.AfterCall < 0 {
			return Policy{}, fmt.Errorf("revocation %s requires nonnegative after_call", revocation.Permission)
		}
		if revoked[revocation.Permission] {
			return Policy{}, fmt.Errorf("duplicate revocation for %s", revocation.Permission)
		}
		revoked[revocation.Permission] = true
		policy.Revocations = append(policy.Revocations, Revocation{Permission: revocation.Permission, AfterCall: *revocation.AfterCall})
	}
	sort.Slice(policy.Revocations, func(i, j int) bool { return policy.Revocations[i].Permission < policy.Revocations[j].Permission })

	if _, err := policy.CanonicalJSON(); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

func known(permission string) bool {
	for _, value := range permissions {
		if value == permission {
			return true
		}
	}
	return false
}

func uniqueSorted(kind string, list []string) ([]string, error) {
	if len(list) == 0 {
		return nil, nil
	}
	out := append([]string(nil), list...)
	sort.Strings(out)
	for i := 1; i < len(out); i++ {
		if out[i] == out[i-1] {
			return nil, fmt.Errorf("duplicate %s %q", kind, out[i])
		}
	}
	return out, nil
}

func (p Policy) CanonicalJSON() ([]byte, error) { return json.Marshal(p) }

func (p Policy) Digest() string {
	encoded, err := p.CanonicalJSON()
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
