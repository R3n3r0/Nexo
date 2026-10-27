package domain

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// covers: S-BKT-12
func TestParseReplicationConfig(t *testing.T) {
	known := func(arn string) bool { return arn == "arn:a" || arn == "arn:b" }
	doc := `<ReplicationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Role>arn:aws:iam::1:role/r</Role>
  <Rule><Priority>1</Priority><Status>Enabled</Status><Filter><Prefix>logs/</Prefix></Filter>
    <DeleteMarkerReplication><Status>Enabled</Status></DeleteMarkerReplication><Destination><Bucket>arn:a</Bucket></Destination></Rule>
  <Rule><ID>tagged</ID><Priority>5</Priority><Status>Enabled</Status>
    <Filter><And><Prefix>logs/</Prefix><Tag><Key>k</Key><Value>v</Value></Tag></And></Filter>
    <Destination><Bucket>arn:a</Bucket></Destination></Rule>
  <Rule><ID>off</ID><Priority>2</Priority><Status>Disabled</Status><Filter><Prefix></Prefix></Filter>
    <Destination><Bucket>arn:b</Bucket></Destination></Rule>
</ReplicationConfiguration>`
	cfg, err := ParseReplicationConfig([]byte(doc), known)
	require.NoError(t, err)
	require.Len(t, cfg.Rules, 3)
	assert.Equal(t, "rule-1", cfg.Rules[0].ID)
	assert.True(t, cfg.Rules[0].ReplicatesDeleteMarkers())
	assert.False(t, cfg.Rules[0].ReplicatesDeletes())
	assert.False(t, cfg.Rules[1].ReplicatesDeleteMarkers(), "options default to Disabled")
	assert.True(t, cfg.UsesTags())
	assert.Equal(t, []string{"arn:a", "arn:b"}, cfg.DestinationARNs())

	// The rule of the highest priority wins per destination; a disabled
	// rule hides its destination.
	dests := cfg.Destinations("logs/x", map[string]string{"k": "v"})
	require.Len(t, dests, 1)
	assert.Equal(t, "tagged", dests[0].ID)
	dests = cfg.Destinations("logs/x", nil)
	require.Len(t, dests, 1)
	assert.Equal(t, "rule-1", dests[0].ID)
	assert.Empty(t, cfg.Destinations("other", nil))

	again, err := cfg.Encode()
	require.NoError(t, err)
	round, err := ParseReplicationConfig(again, known)
	require.NoError(t, err)
	assert.Equal(t, cfg.Rules, round.Rules)

	for name, bad := range map[string]string{
		"no rule":           `<ReplicationConfiguration></ReplicationConfiguration>`,
		"bad status":        `<ReplicationConfiguration><Rule><Status>On</Status><Destination><Bucket>arn:a</Bucket></Destination></Rule></ReplicationConfiguration>`,
		"no destination":    `<ReplicationConfiguration><Rule><Status>Enabled</Status></Rule></ReplicationConfiguration>`,
		"not xml":           `{`,
		"prefix and filter": `<ReplicationConfiguration><Rule><Status>Enabled</Status><Prefix>a</Prefix><Filter><Prefix>a</Prefix></Filter><Destination><Bucket>arn:a</Bucket></Destination></Rule></ReplicationConfiguration>`,
	} {
		_, err := ParseReplicationConfig([]byte(bad), known)
		require.ErrorIs(t, err, ErrMalformedXML, name)
	}
	for name, bad := range map[string]string{
		"unknown target": `<ReplicationConfiguration><Rule><Status>Enabled</Status><Destination><Bucket>arn:z</Bucket></Destination></Rule></ReplicationConfiguration>`,
		"same priority": `<ReplicationConfiguration><Rule><ID>a</ID><Status>Enabled</Status><Filter><Prefix>a</Prefix></Filter><Destination><Bucket>arn:a</Bucket></Destination></Rule>` +
			`<Rule><ID>b</ID><Status>Enabled</Status><Filter><Prefix>b</Prefix></Filter><Destination><Bucket>arn:a</Bucket></Destination></Rule></ReplicationConfiguration>`,
		"duplicate id": `<ReplicationConfiguration><Rule><ID>a</ID><Status>Enabled</Status><Destination><Bucket>arn:a</Bucket></Destination></Rule>` +
			`<Rule><ID>a</ID><Status>Enabled</Status><Destination><Bucket>arn:a</Bucket></Destination></Rule></ReplicationConfiguration>`,
	} {
		_, err := ParseReplicationConfig([]byte(bad), known)
		require.True(t, errors.Is(err, ErrInvalidArgument), "%s: %v", name, err)
	}
}

// covers: S-BKT-12
func TestOverallReplicationStatus(t *testing.T) {
	c, p, f := ReplicationCompleted, ReplicationPending, ReplicationFailed
	assert.Equal(t, ReplicationStatus(""), OverallReplicationStatus(nil))
	assert.Equal(t, c, OverallReplicationStatus(map[string]ReplicationStatus{"a": c, "b": c}))
	assert.Equal(t, p, OverallReplicationStatus(map[string]ReplicationStatus{"a": c, "b": p}))
	assert.Equal(t, f, OverallReplicationStatus(map[string]ReplicationStatus{"a": f, "b": p}))
}
