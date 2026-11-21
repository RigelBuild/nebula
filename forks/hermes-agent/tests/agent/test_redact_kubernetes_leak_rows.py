"""Round-2 review rows for the Kubernetes pass (RIG-11725)."""

import json
import time

import pytest

from agent.redact import redact_sensitive_text as r, redact_terminal_output

V = "zzqq" + "-fixture-leak-value-77"
K = "keep-visible-r5"
LA = "kubectl.kubernetes.io/last-applied-configuration"
PEM_BODY = "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC" + V
sec = {"apiVersion": "v1", "data": {"k": V}, "kind": "Secret", "metadata": {"name": "s"}, "type": "Opaque"}
la_sec = dict(sec)
la_sec["metadata"] = {"name": "s", "annotations": {LA: json.dumps(sec) + "\n"}}


@pytest.fixture(autouse=True)
def _ensure_redaction_enabled(monkeypatch):
    monkeypatch.delenv("HERMES_REDACT_SECRETS", raising=False)
    monkeypatch.setattr("agent.redact._REDACT_ENABLED", True)


def _modes(t):
    return [r(t, force=True), r(t), r(t, force=True, file_read=True, secret_file=True),
            r(t, force=True, code_file=True)]


LEAK_ROWS = {
 # value split across lines (YAML folded/plain continuation, JSON can't)
 "yaml_data_plain_multiline": f"apiVersion: v1\nkind: Secret\ndata:\n  k: {V[:10]}\n    {V[10:]}\n",
 "yaml_data_folded": f"apiVersion: v1\nkind: Secret\ndata:\n  k: >-\n    {V}\n",
 "yaml_data_dq_multiline": f'apiVersion: v1\nkind: Secret\ndata:\n  k: "abc\n    {V}"\n',
 "yaml_stringdata_dq_multiline": f'apiVersion: v1\nkind: Secret\nstringData:\n  k: "abc\n    {V}"\n',
 "yaml_data_sq_multiline": f"apiVersion: v1\nkind: Secret\ndata:\n  k: 'abc\n    {V}'\n",
 # nested Lists
 "json_list_in_list": json.dumps({"apiVersion": "v1", "kind": "List", "items": [{"apiVersion": "v1", "kind": "List", "items": [sec]}]}),
 "json_list_in_list_pretty_trunc": json.dumps({"apiVersion": "v1", "kind": "List", "items": [{"apiVersion": "v1", "kind": "List", "items": [sec]}]}, indent=4)[:-60],
 "json_secretlist_in_list": json.dumps({"apiVersion": "v1", "kind": "List", "items": [{"apiVersion": "v1", "kind": "SecretList", "items": [{"data": {"k": V}, "metadata": {"name": "s"}}]}]}),
 "json_secretlist_in_list_trunc": json.dumps({"apiVersion": "v1", "kind": "List", "items": [{"apiVersion": "v1", "kind": "SecretList", "items": [{"data": {"k": V}, "metadata": {"name": "s"}}]}]})[:-30],
 "json_secretlist_items_nokind_complete": json.dumps({"apiVersion": "v1", "kind": "SecretList", "items": [{"data": {"k": V}, "metadata": {"name": "s"}}]}),
 "json_secretlist_items_kind_first_after": '{"items":[{"data":{"k":"' + V + '"},"metadata":{"name":"s"}}],"kind":"SecretList"}',
 "yaml_secretlist": f"apiVersion: v1\nkind: SecretList\nitems:\n- data:\n    k: {V}\n  metadata:\n    name: s\n",
 "yaml_secretlist_kind_last": f"apiVersion: v1\nitems:\n- data:\n    k: {V}\n  metadata:\n    name: s\nkind: SecretList\n",
 "yaml_list_in_list": f"apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: List\n  items:\n  - apiVersion: v1\n    data:\n      k: {V}\n    kind: Secret\n",
 "yaml_list_items_no_api": f"apiVersion: v1\nkind: List\nitems:\n- data:\n    k: {V}\n  kind: Secret\n",
 # stringData
 "json_stringdata_only": json.dumps({"apiVersion": "v1", "kind": "Secret", "stringData": {"k": V}}),
 "json_stringdata_trunc": json.dumps({"apiVersion": "v1", "stringData": {"k": V}})[:-3],
 "json_stringdata_nested_obj": '{"apiVersion":"v1","kind":"Secret","stringData":{"k":{"inner":"' + V + '"}}}',
 "json_data_number_bool": '{"apiVersion":"v1","kind":"Secret","data":{"k":' + "1234567890123" + "}}",
 # cut mid-key
 "json_cut_mid_kind_key": json.dumps(sec)[: json.dumps(sec).index('"kind"') + 3],
 "json_cut_mid_data_key": '{"apiVersion":"v1","data":{"k":"' + V + '","ot',
 "json_cut_after_colon_kind": '{"apiVersion":"v1","data":{"k":"' + V + '"},"kind":',
 "json_cut_mid_kind_value": '{"apiVersion":"v1","data":{"k":"' + V + '"},"kind":"Sec',
 "json_cut_kind_value_cm_prefix": '{"apiVersion":"v1","data":{"k":"' + V + '"},"kind":"Conf',
 "yaml_cut_mid_kind": f"apiVersion: v1\ndata:\n  k: {V}\nki",
 "yaml_cut_kind_prefix": f"apiVersion: v1\ndata:\n  k: {V}\nkind: Sec",
 # last-applied with escaped quotes
 "la_json_pretty": json.dumps(la_sec, indent=4),
 "la_json_value_with_quote": json.dumps({"apiVersion": "v1", "kind": "Secret", "metadata": {"annotations": {LA: json.dumps({"apiVersion": "v1", "kind": "Secret", "stringData": {"k": V + '"x\\y'}})}}}),
 "la_in_configmap_wrap_secret": json.dumps({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"annotations": {LA: json.dumps(sec)}}}),
 "la_in_pod_with_env": json.dumps({"apiVersion": "v1", "kind": "Pod", "metadata": {"annotations": {LA: json.dumps({"spec": {"containers": [{"env": [{"name": "DB_PASSWORD", "value": V}]}]}})}}}),
 "la_cut": json.dumps(la_sec)[: json.dumps(la_sec).index(V) + 30],
 "la_yaml_singleline": f"apiVersion: v1\nkind: Secret\nmetadata:\n  annotations:\n    {LA}: '{json.dumps(sec)}'\n",
 "la_yaml_in_cm": f"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  annotations:\n    {LA}: |\n      {json.dumps(sec)}\ndata:\n  x: y\n",
 "la_yaml_in_pod_env": f"apiVersion: v1\nkind: Pod\nmetadata:\n  annotations:\n    {LA}: |\n      " + json.dumps({"spec": {"containers": [{"env": [{"name": "DB_PASSWORD", "value": V}]}]}}) + "\n",
 "la_unicode_escape_key": '{"kind":"Secret","metadata":{"annotations":{"' + LA + '":"{\\"kind\\":\\"Secret\\",\\"d\\u0061ta\\":{\\"k\\":\\"' + V + '\\"}}"}}}',
 # describe
 "describe_env_block": f"Containers:\n  app:\n    Environment:\n      DB_PASSWORD:  {V}\n      LOG: info\n",
 "describe_env_from_and_env": f"    Environment Variables from:\n      s  Secret  Optional: false\n    Environment:\n      API_TOKEN:  {V}\n",
 "describe_env_tab": f"    Environment:\n\tDB_PASSWORD:\t{V}\n",
 "describe_env_multiline_value": f"    Environment:\n      DB_PASSWORD:  line1\n                    {V}\n",
 "describe_secret": "Name: s\nType: Opaque\n\nData\n====\nk:  22 bytes\n",
 "describe_env_crlf": f"    Environment:\r\n      DB_PASSWORD:  {V}\r\n",
 # _URL etc names
 "env_SECRET_ID": f"env:\n- name: CLIENT_SECRET_ID\n  value: {V}\n",
 "env_KEY_ID_aws": f"env:\n- name: AWS_SECRET_ACCESS_KEY_ID\n  value: {V}\n",
 # other env shapes
 "json_env_value_before_name": '{"spec":{"containers":[{"env":[{"value":"' + V + '","name":"DB_PASSWORD"}]}]}}',
 "json_env_trunc_value_before_name": '{"spec":{"containers":[{"env":[{"value":"' + V,
 "json_env_pretty_list": json.dumps({"apiVersion": "apps/v1", "kind": "Deployment", "spec": {"template": {"spec": {"containers": [{"env": [{"name": "DB_PASSWORD", "value": V}]}]}}}}, indent=2),
 "json_env_after_text_line": 'output: {"env":[{"name":"DB_PASSWORD","value":"' + V + '"}]}',
 "jsonpath_env_after_prefix": 'env=[{"name":"DB_PASSWORD","value":"' + V + '"}]',
 "yaml_env_value_first": f"env:\n- value: {V}\n  name: DB_PASSWORD\n",
 "yaml_env_name_after_valueFrom_sibling": f"env:\n- name: DB_PASSWORD\n  value: {V}\n  other: x\n",
 "yaml_env_dq_multiline": f'env:\n- name: DB_PASSWORD\n  value: "abc\n    {V}"\n',
 "yaml_env_indent_deeper_value": f"env:\n  - name: DB_PASSWORD\n    value: {V}\n",
 "yaml_env_flow_seq_compact": f"env: [{{name: DB_PASSWORD, value: '{V}'}}]\n",
 "yaml_env_flow_value_with_comma": f'env: [{{name: DB_PASSWORD, value: "a,{V}"}}]\n',
 "gomap_value_with_space": f"[map[name:DB_PASSWORD value:abc {V}]]",
 # helm values
 "yaml_data_with_dash_items": f"apiVersion: v1\nkind: Secret\ndata:\n  - {V}\n",
 "yaml_data_tab_indent_after_space": f"apiVersion: v1\nkind: Secret\ndata:\n  a: x\n\tk: {V}\n",
 "yaml_kind_quoted": f'apiVersion: v1\nkind: "Secret"\ndata:\n  k: {V}\n',
 "yaml_kind_comment": f"apiVersion: v1\nkind: Secret # s\ndata:\n  k: {V}\n",
 "yaml_data_indent_zero_list": f"items:\n- apiVersion: v1\n  kind: Secret\n  data:\n    k: {V}\n",
 "yaml_data_doc_sep_mid": f"---\napiVersion: v1\ndata:\n  k: {V}\n---\nkind: Secret\n",
 "yaml_data_anchor": f"apiVersion: v1\nkind: Secret\ndata: &d\n  k: {V}\n",
 "yaml_data_tag": f"apiVersion: v1\nkind: Secret\ndata: !!map\n  k: {V}\n",
 "yaml_data_comment_after_key": f"apiVersion: v1\nkind: Secret\ndata: # c\n  k: {V}\n",
 "yaml_kubectl_prefixed_lines": f"$ kubectl get secret s -o yaml\napiVersion: v1\ndata:\n  k: {V}\nkind: Secret\n",
 "json_kind_secret_ns_prefix_line": f'secret.json: {{"apiVersion":"v1","data":{{"k":"{V}"}},"kind":"Secret"}}',
 "json_kind_secret_escaped_name": '{"apiVersion":"v1","d\\u0061ta":{"k":"' + V + '"},"kind":"Secret"}',
 "json_kind_secret_escaped_kind": '{"apiVersion":"v1","data":{"k":"' + V + '"},"kind":"S\\u0065cret"}',
 "json_crlf_pretty": json.dumps(sec, indent=2).replace("\n", "\r\n"),
 "json_apiversion_nonk8s_kind_secret": '{"apiVersion":"2","kind":"Secret","data":{"k":"' + V + '"}}',
}
LEAK_ROWS_2 = {
 "raw_api_sorted_secretlist": '{"apiVersion":"v1","items":[{"data":{"k":"' + V + '"},"metadata":{"name":"s"},"type":"Opaque"}],"kind":"SecretList","metadata":{}}',
 "raw_api_sorted_secretlist_pretty": json.dumps({"apiVersion": "v1", "items": [{"data": {"k": V}, "metadata": {"name": "s"}}], "kind": "SecretList", "metadata": {}}, indent=2, sort_keys=True),
 "raw_api_sorted_secretlist_yaml": f"apiVersion: v1\nitems:\n- data:\n    k: {V}\n  metadata:\n    name: s\nkind: SecretList\nmetadata: {{}}\n",
 "raw_api_unsorted_secretlist": '{"kind":"SecretList","apiVersion":"v1","metadata":{},"items":[{"metadata":{"name":"s"},"data":{"k":"' + V + '"},"type":"Opaque"}]}',
 "jsonpath_range_env": 'web\t[{"name":"DB_PASSWORD","value":"' + V + '"}]\n',
 "jsonpath_range_env2": 'web [{"name":"LOG","value":"x"},{"name":"API_TOKEN","value":"' + V + '"}]\n',
 "describe_env_pem": f"    Environment:\n      TLS_PRIVATE_KEY:  -----BEGIN PRIVATE KEY-----\n                        {PEM_BODY}\n                        -----END PRIVATE KEY-----\n",
 "describe_env_multiline": f"    Environment:\n      DB_PASSWORD:  line1\n      {V}\n",
 "env_VAULT_SECRET_ID": f"env:\n- name: VAULT_SECRET_ID\n  value: {V}\n",
 "env_APPROLE_SECRET_ID_json": '{"spec":{"containers":[{"env":[{"name":"APPROLE_SECRET_ID","value":"' + V + '"}]}]}}',
 "env_CLIENT_SECRET_ID_describe": f"    Environment:\n      CLIENT_SECRET_ID:  {V}\n",
 "json_env_pretty_after_cmd_line": '$ kubectl get pod p -o jsonpath="{.spec.containers[0].env}"\n[{"name":"DB_PASSWORD","value":"' + V + '"}]',
 "secret_yaml_last_applied_dq_scalar": 'apiVersion: v1\nkind: Secret\nmetadata:\n  annotations:\n    kubectl.kubernetes.io/last-applied-configuration: "{\\"apiVersion\\":\\"v1\\",\\"data\\":{\\"k\\":\\"' + V + '\\"},\\"kind\\":\\"Secret\\"}"\ndata:\n  k: eA==\n',
 # a suffix alone does not unmask a secret-word name
 "env_API_TOKENS": f"env:\n- name: API_TOKENS\n  value: {V}\n",
 "env_ACCESS_TOKENS_json": json.dumps({"spec": {"containers": [{"env": [{"name": "ACCESS_TOKENS", "value": V + ",o"}]}]}}),
 "env_AUTH_TOKEN_TYPE_describe": f"    Environment:\n      AUTH_TOKEN_TYPE:  {V}\n",
 "env_SECRET_KEY_NAME": f"env:\n- name: SECRET_KEY_NAME\n  value: {V}\n",
 "env_DB_PASSWORD_ENC_MODE_describe": f"    Environment:\n      DB_PASSWORD_ENC_MODE:  {V}\n",
 "env_DB_PASSWORD_ENC_MODE_json": json.dumps({"spec": {"containers": [{"env": [{"name": "DB_PASSWORD_ENC_MODE", "value": V}]}]}}),
 # a *_FILE value that is not an absolute path is the secret itself
 "env_DB_PASSWORD_FILE_inline": f"env:\n- name: DB_PASSWORD_FILE\n  value: {V}\n",
 "env_API_KEY_FILE_json": json.dumps({"spec": {"containers": [{"env": [{"name": "API_KEY_FILE", "value": V}]}]}}),
 "env_API_KEY_FILE_describe": f"    Environment:\n      API_KEY_FILE:  {V}\n",
}
KEEP_ROWS = {
 "env_MAX_TOKENS_yaml": f'env:\n- name: MAX_TOKENS\n  value: "{K}"\n',
 "env_TOKENIZERS_PARALLELISM": f'env:\n- name: TOKENIZERS_PARALLELISM\n  value: "{K}"\n',
 "env_PASSWORD_MIN_LENGTH": f'env:\n- name: PASSWORD_MIN_LENGTH\n  value: "{K}"\n',
 "env_AUTH_ENABLED": f'env:\n- name: AUTH_ENABLED\n  value: "{K}"\n',
 "env_AUTH_MODE_json": '{"spec":{"containers":[{"env":[{"name":"AUTH_MODE","value":"' + K + '"}]}]}}',
 "env_TOTAL_TOKENS": f'env:\n- name: TOTAL_TOKENS\n  value: "{K}"\n',
 "env_NUM_TOKENS": f'env:\n- name: NUM_TOKENS\n  value: "{K}"\n',
 "env_MAX_TOKENS_json": json.dumps({"spec": {"containers": [{"env": [{"name": "MAX_TOKENS", "value": K}]}]}}),
 "env_DB_PASSWORD_FILE_path": f"env:\n- name: DB_PASSWORD_FILE\n  value: /run/secrets/{K}\n",
 "env_API_KEY_FILE_path_json": json.dumps({"spec": {"containers": [{"env": [{"name": "API_KEY_FILE", "value": "/etc/" + K}]}]}}),
 "env_SECRET_ROTATION_DAYS": f'env:\n- name: SECRET_ROTATION_DAYS\n  value: "{K}"\n',
 "jsonl_metric_tokens": '{"name":"llm.total_tokens","value":"' + K + '"}\n',
 "python_call_dict_line": 'add(\n    {"name": "token", "value": "' + K + '"},\n)\n',
 "python_list_dict_line": 'FIELDS = [\n    {"name": "password", "value": "' + K + '"},\n]\n',
 "api_envelope_v1": '{"apiVersion":"v1","data":{"title":"' + K + '"}}',
 "api_envelope_v2_pretty": json.dumps({"apiVersion": "v2", "data": {"items": [{"title": K}], "title": K}}, indent=2),
 "api_envelope_v1_nested_kind": '{"apiVersion":"v1","data":{"kind":"user","title":"' + K + '"}}',
 "kubeconfig": f"apiVersion: v1\nclusters:\n- cluster:\n    server: https://{K}\n  name: c\nkind: Config\n",
 "chart_yaml": f"apiVersion: v2\nname: app\ndescription: {K}\ndata:\n  x: {K}\n",
 "configmap_list_sorted": json.dumps({"apiVersion": "v1", "items": [{"data": {"k": K}, "metadata": {"name": "c"}}], "kind": "ConfigMapList"}),
 "configmap_list_sorted_yaml": f"apiVersion: v1\nitems:\n- data:\n    k: {K}\n  metadata:\n    name: c\nkind: ConfigMapList\n",
 "log_line_value_colon": f"2026-01-01 INFO value: {K}\nname: token\n",
 "yaml_name_value_generic": f"params:\n- name: token-url\n  value: {K}\n",
 "markdown_value_colon": f"- name: api key rotation\n  value: {K}\n",
 "go_struct_print": f"{{Name:token Value:{K}}}",
 "gomap_tokens": f"map[name:max_tokens value:{K}]",
 "helm_flow_env_plain": f"env: [{{name: LOG_LEVEL, value: {K}}}]\n",
 "yaml_data_secret_kind_in_other_doc_no_sep": f"apiVersion: v1\nkind: Secret\ndata:\n  a: eA==\napiVersion: v1\nkind: ConfigMap\ndata:\n  b: {K}\n",
 "json_secret_then_cm_newline": '{"apiVersion":"v1","kind":"Secret","data":{"a":"x"}}\n{"apiVersion":"v1","kind":"ConfigMap","data":{"b":"' + K + '"}}',
}
ALL_LEAK = {**LEAK_ROWS, **LEAK_ROWS_2}


@pytest.mark.parametrize("text", ALL_LEAK.values(), ids=ALL_LEAK.keys())
def test_secret_masked(text):
    for out in _modes(text):
        assert V not in out, out


@pytest.mark.parametrize("text", KEEP_ROWS.values(), ids=KEEP_ROWS.keys())
def test_setting_value_kept(text):
    for out in [r(text, force=True), r(text), r(text, force=True, code_file=True), r(text, file_read=True)]:
        assert K in out, out


def test_describe_pem_env_masked_in_terminal_output():
    text = ("    Environment:\n      TLS_PRIVATE_KEY:  -----BEGIN PRIVATE KEY-----\n"
            f"                        {PEM_BODY}\n                        -----END PRIVATE KEY-----\n")
    out = redact_terminal_output(text, "kubectl describe pod web", force=True)
    assert V not in out and "MIIEvQIBADAN" not in out, out


def test_file_read_uses_non_reusable_sentinel():
    out = r(f"apiVersion: v1\nkind: Secret\ndata:\n  k: {V}\n", file_read=True)
    assert "k: «redacted-secret»" in out, out


def test_one_megabyte_open_brackets_under_a_second():
    text = '{"apiVersion":"v1","data":' + "[" * (1 << 20)
    start = time.perf_counter()
    r(text, force=True)
    assert time.perf_counter() - start < 1.0


@pytest.mark.parametrize("unit", ["& ", "!a "])
def test_prop_chain_scales_linearly(unit):
    def cost(n):
        text = "data: " + unit * (n // len(unit))
        start = time.perf_counter()
        r(text, force=True)
        return time.perf_counter() - start

    cost(100_000)  # warm up
    small, big = min(cost(100_000) for _ in range(3)), min(cost(1_000_000) for _ in range(3))
    assert big / max(small, 1e-4) < 25, (small, big)
