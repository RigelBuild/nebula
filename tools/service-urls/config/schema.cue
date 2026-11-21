// Deployment values for the service links tool. Every field is required; the
// schema gives none a default. Paths are relative to the workspace root.

#Owners: close({
	// Inventory rows whose source starts with this prefix belong to the Kubernetes endpoints.
	kubernetes: string & !=""
	// Rows owned by the internal DNS records.
	dns: string & !=""
	// Rows owned by the Cloudflare hosts.
	cloudflare: string & !=""
	// Rows owned by the Worker custom domains.
	workers: string & !=""
})

#Config: close({
	// Suffixes whose hosts are reachable only on the tailnet, such as "internal.example.com".
	internalHostSuffixes: [string & !="", ...string & !=""]
	// Domain that Tailscale uses for full tailnet host names.
	tailscaleDomain: string & !=""
	// Zone appended to each internal DNS record name.
	dnsZone: string & !=""
	// Inventory directory that holds groups.toml and the link tables.
	inventoryPath: string & !=""
	// Generated service links page.
	linksPagePath: string & !=""
	// Generated Glance bookmarks config.
	glancePath: string & !=""
	// Inventory rows with exactly this source are the Glance portal's own and stay off the bookmarks.
	portalSource: string & !=""
	// Tailnet inventory: the MagicDNS suffix and the fleet node names.
	tailnetPath: string & !=""
	// Kubernetes manifests rendered into one YAML union.
	unionPath: string & !=""
	// Internal DNS record names.
	dnsRecordsPath: string & !=""
	// Managed GitHub repository list.
	managedReposPath: string & !=""
	// Cloudflare Access and proxied host list.
	cloudflareHostsPath: string & !=""
	// Pulumi source that declares the Worker custom domains.
	workerDomainsPath: string & !=""
	owners:            #Owners
})
