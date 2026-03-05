# Future Enhancement: Structured ACL Permissions

**Status:** Proposed Future Enhancement
**Related:** [INITIAL_CRD_DESIGN_STRATEGY.md](./INITIAL_CRD_DESIGN_STRATEGY.md)

---

## Overview

This document proposes structured permission fields to abstract Valkey ACL syntax, making it easier for users to configure permissions without memorizing raw ACL commands.

The initial CRD design uses a raw `permissions` string (e.g., `"+@all -@admin ~app:*"`). This enhancement adds structured fields that generate the ACL string, with an escape hatch for advanced use cases.

## Motivation

**Problems with raw permissions string:**
- Users must memorize Valkey ACL syntax
- Easy to make syntax errors (caught only at runtime)
- Read/write key patterns use cryptic syntax (`%R~`, `%W~`)
- No CRD-level validation

**Benefits of structured permissions:**
- Self-documenting field names
- CRD validation catches errors at apply time
- Abstracts Valkey-specific syntax
- Still allows raw escape hatch for advanced users

## Proposed Schema

```yaml
users:
  - name: app-user
    enabled: true
    passwordSecret:
      name: app-credentials
      keys: [password]

    # Structured permissions (validated by CRD)
    commands:
      # Command categories (@all, @read, @write, @admin, etc.)
      # Individual commands (get, set, ping, etc.)
      # Subcommands (client|setname, config|get, etc.)
      # +kubebuilder:validation:Items:Pattern=^[@a-z|]+$
      allow: ["@read", "@write", "@connection"]
      deny: ["@admin", "@dangerous"]

    keys:
      # Full access - maps to Valkey: ~pattern
      readWrite: ["app:*", "cache:*"]
      # Read-only access - maps to Valkey: %R~pattern
      readOnly: ["shared:*", "config:*"]
      # Write-only access - maps to Valkey: %W~pattern
      writeOnly: ["logs:*", "metrics:*"]

    channels:
      # Pub/Sub channel patterns - maps to Valkey: &pattern
      # Note: Valkey does not support separate subscribe/publish permissions
      patterns: ["notifications:*", "events:*"]

    # Raw escape hatch - appended to generated ACL (optional)
    # Use for advanced features not abstracted by structured fields
    additionalPermissions: "+client|setname +debug|sleep"

  - name: legacy-user
    nopass: true
    # Raw permissions string (alternative to structured)
    # Use when structured fields don't meet requirements
    permissions: "+@all -@admin ~* &*"
```

## Field Definitions

### commands

Controls which Valkey commands the user can execute.

| Field | Type | Description |
|-------|------|-------------|
| `allow` | []string | Commands/categories to allow |
| `deny` | []string | Commands/categories to deny |

**Supported formats:**
- `@category` - Command category (e.g., `@read`, `@write`, `@admin`, `@dangerous`)
- `command` - Individual command (e.g., `get`, `set`, `ping`)
- `command|subcommand` - Subcommand (e.g., `client|setname`, `config|get`)

### keys

Controls which keys the user can access.

| Field | Type | Valkey ACL | Description |
|-------|------|------------|-------------|
| `readWrite` | []string | `~pattern` | Full access (read + write) |
| `readOnly` | []string | `%R~pattern` | Read-only access |
| `writeOnly` | []string | `%W~pattern` | Write-only access |

**Pattern format:** Glob-style patterns (e.g., `app:*`, `cache:user:*`, `*`)

### channels

Controls which Pub/Sub channels the user can access.

| Field | Type | Valkey ACL | Description |
|-------|------|------------|-------------|
| `patterns` | []string | `&pattern` | Channel access patterns |

**Note:** Valkey does not support separate subscribe/publish permissions. The `patterns` field grants access to both operations.

### additionalPermissions

Raw ACL string appended to the generated permissions. Use as an escape hatch for:
- Features not yet abstracted by structured fields
- Advanced ACL options (selectors, etc.)
- Overriding structured permissions (Valkey ACL is left-to-right, last wins)

### permissions

Fully raw alternative to structured fields. Use when:
- Migrating existing ACL configurations
- Structured fields don't meet requirements
- User prefers raw ACL syntax

## Validation Rules

**Field combinations:**
- Can use structured fields (`commands`, `keys`, `channels`), raw `permissions`, or both
- If both structured and `additionalPermissions` are used, structured generates base ACL, `additionalPermissions` appends
- `permissions` field is the fully raw alternative (bypasses structured generation)

**Field validation:**
- `commands.allow` and `commands.deny` entries must match pattern `^[@a-z|]+$`
- Key and channel patterns must be valid glob patterns

## ACL Generation Order

When using structured fields, the operator generates the ACL string in this order:

1. **Commands:** `+@allow... -@deny...`
2. **Keys:** `~readWrite... %R~readOnly... %W~writeOnly...`
3. **Channels:** `&patterns...`
4. **Additional:** `additionalPermissions` appended last

**Important:** Valkey ACL uses left-to-right, cumulative evaluation. Later rules override earlier ones. This means `additionalPermissions` can override structured permissions if needed.

### Example

```yaml
commands:
  allow: ["@all"]
  deny: ["@admin"]
keys:
  readWrite: ["app:*"]
  readOnly: ["shared:*"]
channels:
  patterns: ["events:*"]
additionalPermissions: "+config|get"
```

**Generated ACL:**
```
+@all -@admin ~app:* %R~shared:* &events:* +config|get
```

**Result:** All commands except `@admin`, but `config|get` is allowed (last wins).

## Migration Path

Existing users with raw `permissions` strings continue to work unchanged. Users can gradually migrate to structured fields:

1. **Phase 1:** Continue using `permissions` string
2. **Phase 2:** Use structured fields for new users
3. **Phase 3:** Migrate existing users to structured fields (optional)

## Implementation Notes

### Operator Behavior

1. If `permissions` is set, use it directly (no generation)
2. If structured fields are set, generate ACL string:
   - Process `commands.allow` → `+entry` for each
   - Process `commands.deny` → `-entry` for each
   - Process `keys.readWrite` → `~pattern` for each
   - Process `keys.readOnly` → `%R~pattern` for each
   - Process `keys.writeOnly` → `%W~pattern` for each
   - Process `channels.patterns` → `&pattern` for each
   - Append `additionalPermissions` if set
3. Apply generated or raw ACL to Valkey

### Webhook Validation

```go
func (u *User) ValidatePermissions() error {
    hasStructured := u.Commands != nil || u.Keys != nil || u.Channels != nil
    hasRaw := u.Permissions != ""

    // Both is allowed (raw overrides structured intent)
    // Neither requires additionalPermissions or is an error
    if !hasStructured && !hasRaw && u.AdditionalPermissions == "" {
        return errors.New("user must have permissions, structured fields, or additionalPermissions")
    }

    // Validate command patterns
    if u.Commands != nil {
        pattern := regexp.MustCompile(`^[@a-z|]+$`)
        for _, cmd := range append(u.Commands.Allow, u.Commands.Deny...) {
            if !pattern.MatchString(cmd) {
                return fmt.Errorf("invalid command pattern: %s", cmd)
            }
        }
    }

    return nil
}
```

## References

- [Valkey ACL Documentation](https://valkey.io/topics/acl/)
- Valkey ACL uses left-to-right evaluation; later rules override earlier ones
- Valkey does not support separate subscribe/publish permissions for Pub/Sub channels
