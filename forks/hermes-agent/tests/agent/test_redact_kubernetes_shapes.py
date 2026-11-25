"""Review probe rows for the Kubernetes pass (RIG-11725): leak shapes, keep shapes and perf."""

import time

import pytest

from agent.redact import redact_sensitive_text as r

V = "zzqq" + "-fixture-leak-value-77"
P = "zzqq" + "-fixture-pw-value-88"
K = "keep-visible-r5"
LA = "kubectl.kubernetes.io/last-applied-configuration"


@pytest.fixture(autouse=True)
def _ensure_redaction_enabled(monkeypatch):
    monkeypatch.delenv("HERMES_REDACT_SECRETS", raising=False)
    monkeypatch.setattr("agent.redact._REDACT_ENABLED", True)


def _modes(t):
    return [r(t, force=True), r(t), r(t, force=True, file_read=True, secret_file=True),
            r(t, force=True, code_file=True)]


LEAK_ROWS = {
 "trunc_secret_kind_first": f'{{"kind":"Secret","metadata":{{"name":"s"}},"data":{{"k":"{V}',
 "trunc_full_kubectl_order": f'{{"apiVersion":"v1","kind":"Secret","metadata":{{"name":"s","namespace":"d"}},"type":"Opaque","data":{{"token":"{V}',
 "trunc_secret_pretty": f'{{\n  "kind": "Secret",\n  "metadata": {{\n    "name": "s",\n    "labels": {{"a": "b"}}\n  }},\n  "data": {{\n    "k": "{V}',
 "trunc_kubectl_order_no_kind_pretty": f'{{\n    "apiVersion": "v1",\n    "data": {{\n        "k": "{V}',
 "trunc_kubectl_order_no_kind_compact": f'{{"apiVersion":"v1","data":{{"k":"{V}',
 "trunc_kubectl_order_two_keys": f'{{\n    "apiVersion": "v1",\n    "data": {{\n        "a": "x",\n        "k": "{V}',
 "complete_kubectl_order_compact": f'{{"apiVersion":"v1","data":{{"k":"{V}"}},"kind":"Secret","metadata":{{"name":"s"}},"type":"Opaque"}}',
 "complete_kubectl_order_pretty": f'{{\n    "apiVersion": "v1",\n    "data": {{\n        "k": "{V}"\n    }},\n    "kind": "Secret",\n    "metadata": {{\n        "name": "s"\n    }}\n}}',
 "trunc_list_of_secrets": f'{{"apiVersion":"v1","items":[{{"apiVersion":"v1","data":{{"k":"{V}',
 "trunc_secretlist": f'{{"kind":"SecretList","items":[{{"metadata":{{"name":"s"}},"data":{{"k":"{V}',
 "complete_secretlist_nested_items": f'{{"apiVersion":"v1","items":[{{"apiVersion":"v1","data":{{"k":"{V}"}},"kind":"Secret","metadata":{{"name":"s"}}}}],"kind":"List"}}',
 "trunc_stringdata_after_data": f'{{"kind":"Secret","data":{{"a":"b"}},"stringData":{{"k":"{V}',
 "trunc_close_brace_in_string": f'{{"kind":"Secret","metadata":{{"name":"s}}","labels":{{"a":"}}}}}}"}}}},"data":{{"k":"{V}',
 "trunc_data_value_with_brace": f'{{"kind":"Secret","metadata":{{"name":"s"}},"data":{{"a":"}}{{","k":"{V}',
 "trunc_escaped_quote": f'{{"kind":"Secret","metadata":{{"name":"a\\"}}","x":"b\\\\"}},"data":{{"k":"{V}',
 "trunc_two_secrets_second": f'{{"kind":"Secret","metadata":{{"name":"a"}},"data":{{"x":"q"}}}}\n{{"kind":"Secret","metadata":{{"name":"s"}},"data":{{"k":"{V}',
 "annot_last_applied_in_secret": f'{{"kind":"Secret","metadata":{{"annotations":{{"{LA}":"{{\\"apiVersion\\":\\"v1\\",\\"data\\":{{\\"k\\":\\"{V}\\"}},\\"kind\\":\\"Secret\\"}}"}}}}}}',
 "annot_last_applied_cm_wraps_secret": f'{{"kind":"ConfigMap","metadata":{{"annotations":{{"{LA}":"{{\\"kind\\":\\"Secret\\",\\"data\\":{{\\"k\\":\\"{V}\\"}}}}"}}}}}}',
 "yaml_annot_last_applied": f'apiVersion: v1\nkind: Secret\nmetadata:\n  annotations:\n    {LA}: |\n      {{"apiVersion":"v1","data":{{"k":"{V}"}},"kind":"Secret"}}\ndata:\n  k: eA==\n',
 # stray quote
 "oq_then_trunc": f'say "\n{{"kind":"Secret","metadata":{{"name":"s"}},"data":{{"k":"{V}',
 "oq_then_trunc_pretty": f'warn: "x\n{{\n  "kind": "Secret",\n  "metadata": {{"name": "s"}},\n  "data": {{\n    "k": "{V}',
 "oq_apiversion_nokind": f'x"\n{{"apiVersion":"v1","data":{{"k":"{V}',
 "oq_three_quotes": f'a"b"c"\n{{"kind":"Secret","metadata":{{"name":"s"}},"data":{{"k":"{V}',
 # List pod then secret
 "list_pod_then_secret_trunc": f'{{"apiVersion":"v1","items":[{{"apiVersion":"v1","kind":"Pod","metadata":{{"name":"p"}}}},{{"apiVersion":"v1","data":{{"k":"{V}',
 "list_pod_then_secret_complete": f'{{"apiVersion":"v1","items":[{{"apiVersion":"v1","kind":"Pod","metadata":{{"name":"p"}}}},{{"apiVersion":"v1","data":{{"k":"{V}"}},"kind":"Secret","metadata":{{"name":"s"}}}}],"kind":"List"}}',
 "get_pod_secret_real_pretty_trunc": f'$ kubectl get pod,secret -o json | head -c 400\n{{\n    "apiVersion": "v1",\n    "items": [\n        {{\n            "apiVersion": "v1",\n            "kind": "Pod",\n            "metadata": {{\n                "name": "web"\n            }}\n        }},\n        {{\n            "apiVersion": "v1",\n            "data": {{\n                "token": "{V}',
 "list_cm_then_secret_trunc": f'{{"apiVersion":"v1","items":[{{"apiVersion":"v1","data":{{"a":"b"}},"kind":"ConfigMap","metadata":{{"name":"c"}}}},{{"apiVersion":"v1","data":{{"k":"{V}',
 # YAML
 "yaml_data": f'apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\ndata:\n  connection: {V}\n',
 "yaml_data_before_kind": f'apiVersion: v1\ndata:\n  connection: {V}\nkind: Secret\nmetadata:\n  name: s\ntype: Opaque\n',
 "yaml_stringdata": f'apiVersion: v1\nkind: Secret\nstringData:\n  connection: {V}\n',
 "yaml_stringdata_block": f'apiVersion: v1\nkind: Secret\nstringData:\n  config.yaml: |\n    url: http://x\n    blob: {V}\n',
 "yaml_quoted_value": f'kind: Secret\ndata:\n  connection: "{V}"\n',
 "yaml_trunc_no_kind": f'apiVersion: v1\ndata:\n  connection: {V}\n',
 "yaml_list_items_secret": f'apiVersion: v1\nitems:\n- apiVersion: v1\n  data:\n    connection: {V}\n  kind: Secret\n  metadata:\n    name: s\nkind: List\n',
 "yaml_list_pod_then_secret": f'apiVersion: v1\nitems:\n- apiVersion: v1\n  kind: Pod\n  metadata:\n    name: p\n- apiVersion: v1\n  data:\n    connection: {V}\n  kind: Secret\n  metadata:\n    name: s\nkind: List\n',
 "yaml_data_key_with_dot_colon": f'kind: Secret\ndata:\n  "a:b": {V}\n',
 "yaml_flow_map": f'kind: Secret\ndata: {{connection: {V}}}\n',
 "yaml_crlf": f'apiVersion: v1\r\nkind: Secret\r\ndata:\r\n  connection: {V}\r\n',
 "yaml_tab_data": f'kind: Secret\ndata:\n\tconnection: {V}\n',
 "yaml_multiline_plain": f'kind: Secret\ndata:\n  connection:\n    {V}\n',
}
ENV_ROWS = {
 "yaml_deploy_env": f'kind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n      - name: app\n        env:\n        - name: DB_PASSWORD\n          value: {P}\n',
 "yaml_deploy_env_second_container": f'kind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n      - name: a\n        image: x\n      - name: b\n        env:\n        - name: API_TOKEN\n          value: {P}\n',
 "yaml_init_containers": f'spec:\n  initContainers:\n  - name: init\n    env:\n    - name: DB_PASSWORD\n      value: {P}\n',
 "yaml_env_quoted": f'env:\n- name: "DB_PASSWORD"\n  value: "{P}"\n',
 "yaml_env_name_comment": f'env:\n- name: DB_PASSWORD  # db\n  value: {P}\n',
 "yaml_env_with_between_key": f'env:\n- name: DB_PASSWORD\n  # comment\n  value: {P}\n',
 "yaml_env_block_scalar": f'env:\n- name: DB_PASSWORD\n  value: |\n    {P}\n',
 "yaml_env_flow": f'env: [{{name: DB_PASSWORD, value: {P}}}]\n',
 "yaml_env_crlf": f'env:\r\n- name: DB_PASSWORD\r\n  value: {P}\r\n',
 "json_env": f'{{"kind":"Deployment","spec":{{"template":{{"spec":{{"containers":[{{"env":[{{"name":"DB_PASSWORD","value":"{P}"}}]}}]}}}}}}}}',
 "json_env_second_container": f'{{"spec":{{"containers":[{{"env":[{{"name":"A","value":"b"}}]}},{{"name":"c2","env":[{{"name":"API_TOKEN","value":"{P}"}}]}}]}}}}',
 "json_env_initcontainers": f'{{"spec":{{"containers":[],"initContainers":[{{"env":[{{"name":"DB_PASSWORD","value":"{P}"}}]}}]}}}}',
 "json_env_trunc": f'{{"spec":{{"containers":[{{"env":[{{"name":"DB_PASSWORD","value":"{P}',
 "json_env_pretty_trunc": f'{{\n  "env": [\n    {{\n      "name": "DB_PASSWORD",\n      "value": "{P}',
 "json_env_escaped_name": f'{{"name":"DB_PASS\\u0057ORD","value":"{P}"}}',
 "gomap_env": f'[map[name:DB_PASSWORD value:{P}]]',
 "gomap_env_multi": f'[map[name:LOG_LEVEL value:info] map[name:DB_PASSWORD value:{P}]]',
 "gomap_env_reversed": f'[map[value:{P} name:DB_PASSWORD]]',
 "jsonpath_env_json": f'[{{"name":"DB_PASSWORD","value":"{P}"}}]',
 "describe_env": f'Containers:\n  app:\n    Environment:\n      DB_PASSWORD:  {P}\n',
 "describe_env_secretref_variant": f'    Environment:\n      DB_PASSWORD:  <set to the key \'p\' in secret \'s\'>  Optional: false\n      API_KEY:  {P}\n',
 "env_names_misc_DATABASE_URL": f'env:\n- name: DATABASE_URL\n  value: postgres://u:{P}@h/db\n',
 "env_name_AUTH": f'env:\n- name: AUTH\n  value: {P}\n',
 "env_name_PRIVATE_KEY": f'env:\n- name: PRIVATE_KEY\n  value: {P}\n',
 "env_name_DB_PASS": f'env:\n- name: DB_PASS\n  value: {P}\n',
 "env_name_PWD": f'env:\n- name: MYSQL_PWD\n  value: {P}\n',
 "env_name_APIKEY": f'env:\n- name: STRIPE_APIKEY\n  value: {P}\n',
}

KEEP_ROWS = {
 "configmap_yaml": f"apiVersion: v1\nkind: ConfigMap\ndata:\n  connection: {K}\n",
 "configmap_json": f'{{"apiVersion":"v1","kind":"ConfigMap","data":{{"connection":"{K}"}}}}',
 "plain_json_data": f'{{"name":"app","version":"1.0","data":{{"x":"{K}"}}}}',
 "plain_json_data_trunc": f'{{"name":"pkg","apiVersion":"2","data":{{"x":"{K}"',
 "yaml_data_unrelated": f"data:\n  title: {K}\n",
 "env_PATH_yaml": f"env:\n- name: PATH\n  value: /usr/bin:{K}\n",
 "env_PATH_json": f'{{"name":"PATH","value":"/usr/bin:{K}"}}',
 "env_LOG_LEVEL": f"env:\n- name: LOG_LEVEL\n  value: {K}\n",
 "env_KEYCLOAK_URL": f"env:\n- name: KEYCLOAK_URL\n  value: https://{K}.example\n",
 "env_TOKEN_TTL": f"env:\n- name: TOKEN_TTL\n  value: {K}\n",
 "env_SECRET_NAME_ref": f"env:\n- name: VAULT_SECRET_PATH\n  value: kv/{K}\n",
 "env_KEY_FILE": f"env:\n- name: TLS_KEY_FILE\n  value: /etc/{K}\n",
 "html_form": f'<input name="password" value="{K}">',
 "js_literal": f'function f() {{ return {{"name":"b", "value":"{K}"}} }}',
 "js_literal_secretname": f'const opts = [{{"name":"token", "value":"{K}"}}];',
 "har_headers": f'{{"headers":[{{"name":"Content-Type","value":"{K}"}}]}}',
 "crd_schema_trunc": f'{{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","spec":{{"properties":{{"data":{{"type":"object","description":"{K}"',
 "secret_kind_prose_then_json": f'The Secret kind holds data.\n{{"data":{{"x":"{K}"}}}}',
 "secret_doc_then_cm_doc": f"apiVersion: v1\nkind: Secret\ndata:\n  a: eA==\n---\napiVersion: v1\nkind: ConfigMap\ndata:\n  b: {K}\n",
 "secret_list_yaml_then_cm": f"apiVersion: v1\nitems:\n- apiVersion: v1\n  kind: Secret\n  data:\n    a: eA==\n- kind: ConfigMap\n  data:\n    b: {K}\n",
 "secret_json_then_cm_json_sameline_no_apiv": f'{{"kind":"Secret","data":{{"a":"x"}}}} {{"kind":"ConfigMap","data":{{"b":"{K}"}}}}',
 "json_list_secret_cm": f'{{"apiVersion":"v1","items":[{{"apiVersion":"v1","kind":"Secret","data":{{"a":"x"}}}},{{"apiVersion":"v1","kind":"ConfigMap","data":{{"b":"{K}"}}}}]}}',
 "describe_set_literal": f"    Environment:\n      DB_PASSWORD:  <set to the key 'k' in secret '{K}'>\n",
 "python_dict_secret_kind": f'kind = "Secret"\ncfg = {{"data": {{"label": "{K}"}}}}\n',
 "go_map_PATH": f"[map[name:PATH value:{K}]]",
}


@pytest.mark.parametrize("text", LEAK_ROWS.values(), ids=LEAK_ROWS.keys())
def test_secret_data_masked(text):
    for out in _modes(text):
        assert V not in out, out


@pytest.mark.parametrize("text", ENV_ROWS.values(), ids=ENV_ROWS.keys())
def test_secret_env_masked(text):
    for out in _modes(text):
        assert P not in out, out


@pytest.mark.parametrize("text", KEEP_ROWS.values(), ids=KEEP_ROWS.keys())
def test_neutral_value_kept(text):
    for out in [r(text, force=True), r(text), r(text, force=True, code_file=True)]:
        assert K in out, out


KB = 1024


def _rep(unit, n):
    return unit * (-(-n // len(unit)))


PERF_ROWS = {
    "yaml-env-interior-ws": "- name: A\n  value: x" + " " * KB * KB + "y\n",
    "yaml-env-value-first-ws": "- value: x" + " " * KB * KB + "y\n  name: A\n",
    "yaml-env-interior-tab-many": "- name: A\n  value: x" + " \t" * (KB * KB // 2) + "y\n",
    "yaml-env-128k-spaces-secret": "- name: DB_PASSWORD\n  value: x" + " " * 128 * KB + "y\n",
    "json-data-ws": '{"kind":"Secret","data"' + " " * KB * KB + "x",
    "unclosed-objects": '{"kind":"Secret",' + _rep('{"name":"a",', KB * KB),
    "env-secret-unclosed": '{"env":[' + _rep('{"name":"DB_PASSWORD","value":"b",', KB * KB),
    "deep-open": '{"kind":"Secret",' + "{" * (KB * KB),
    "yaml-env-many": _rep("- name: DB_PASSWORD\n  value: x\n", KB * KB),
    "describe-long-line": "    Environment:\n      DB_PASSWORD:" + " " * KB * KB + "x\n",
}


@pytest.mark.parametrize("text", PERF_ROWS.values(), ids=PERF_ROWS.keys())
def test_one_megabyte_shape_under_a_second(text):
    start = time.perf_counter()
    r(text, force=True)
    assert time.perf_counter() - start < 1.0
