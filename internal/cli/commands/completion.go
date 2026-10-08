package commands

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
)

// completionTimeout bounds the requests of the dynamic completion.
const completionTimeout = 3 * time.Second

// completionLimit bounds the number of objects proposed.
const completionLimit = 200

// completeAliasNames completes the names of the aliases.
func completeAliasNames(e *env) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		aliases, err := e.completionAliases(cmd)
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		var out []string
		for _, name := range cli.SortedKeys(aliases) {
			if strings.HasPrefix(name, toComplete) {
				out = append(out, name)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// completeTargets completes remote targets (docs/02 C-UX-01): the aliases
// ("local/"), their buckets ("local/photos/") and the objects and prefixes
// of a bucket; when nothing remote matches the shell completes local
// files.
func completeTargets(e *env) cobra.CompletionFunc {
	return func(cmd *cobra.Command, _ []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		aliases, err := e.completionAliases(cmd)
		if err != nil {
			return nil, cobra.ShellCompDirectiveDefault
		}
		name, rest, hasSlash := strings.Cut(toComplete, "/")
		if !hasSlash {
			var out []string
			for _, a := range cli.SortedKeys(aliases) {
				if strings.HasPrefix(a, toComplete) {
					out = append(out, a+"/")
				}
			}
			if len(out) == 0 {
				return nil, cobra.ShellCompDirectiveDefault
			}
			return out, cobra.ShellCompDirectiveNoSpace
		}
		if _, ok := aliases[name]; !ok {
			return nil, cobra.ShellCompDirectiveDefault
		}
		ctx, cancel := context.WithTimeout(cmdContext(cmd), completionTimeout)
		defer cancel()
		client, err := e.client(ctx, name)
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		bucket, prefix, hasKey := strings.Cut(rest, "/")
		if !hasKey {
			buckets, err := client.ListBuckets(ctx)
			if err != nil {
				return nil, cobra.ShellCompDirectiveError
			}
			var out []string
			for _, b := range buckets {
				if strings.HasPrefix(b.Name, bucket) {
					out = append(out, name+"/"+b.Name+"/")
				}
			}
			return out, cobra.ShellCompDirectiveNoSpace | cobra.ShellCompDirectiveNoFileComp
		}
		dir := prefix[:strings.LastIndex(prefix, "/")+1]
		var out []string
		for o, err := range client.List(ctx, bucket, cli.ListOptions{Prefix: dir}) {
			if err != nil {
				return nil, cobra.ShellCompDirectiveError
			}
			if strings.HasPrefix(o.Key, prefix) {
				out = append(out, name+"/"+bucket+"/"+o.Key)
			}
			if len(out) >= completionLimit {
				break
			}
		}
		sort.Strings(out)
		return out, cobra.ShellCompDirectiveNoSpace | cobra.ShellCompDirectiveNoFileComp
	}
}

// completionAliases loads the aliases during a completion: the global
// flags are parsed but PersistentPreRunE did not run.
func (e *env) completionAliases(cmd *cobra.Command) (map[string]cli.Alias, error) {
	if dir, err := cmd.Flags().GetString(flagConfigDir); err == nil && dir != "" {
		e.configDir = dir
	}
	return e.loadAliases(cmdContext(cmd))
}

func cmdContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}
