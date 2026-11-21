# Example Home Manager agent links for tests. The real source is named by the
# operator's checkout; links_test.go reads this fixture instead.
{
  ".agents/AGENTS.md".source = linkAgent "AGENTS.md";
  ".agents/rules".source = linkAgent "rules";
  ".agents/skills".source = linkAgent "skills";
  ".cotal-config".source = linkAgent "cotal";
  ".omp/agent/extensions".source = linkAgent "extensions";
  ".omp/agent/autolearn".source = linkAgent "autolearn";
  ".omp/agent/agents".source = linkAgent "agents";
  ".omp/agent/config.yml".source = linkAgent "config.yml";
  ".omp/agent/models.yml".source = linkAgent "models.yml";
  ".omp/agent/mcp.json" = { source = linkAgent "mcp.json"; };
}
