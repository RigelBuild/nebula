"""Kubernetes secret values in tool output (RIG-11725).

A Secret's ``data``/``stringData`` values and an env ``value`` under a secret-named ``name``
reach the model through kubectl output. Their keys are neutral (``connection``, ``value``), so
the keyword assignment passes in agent.redact never see them.

Each pass collects ``(start, end)`` spans and the text is rebuilt once. Every pass is linear in
the text: one tokenizer pass for JSON, one pass over lines for YAML and ``kubectl describe``, and
regexes whose repeated parts cannot overlap.

kubectl sorts keys, so ``data`` comes before ``kind``, and output cut off by ``head -c`` ends
inside it. An object's verdict is made when it closes, or at the end of the text: ``kind:
Secret`` masks; any other kind keeps; no kind masks when the object has a Kubernetes
``apiVersion`` or sits in a ``SecretList``. That fails closed on a cut-off Secret and keeps a
ConfigMap and plain JSON visible.
"""

import json
import re

MASK = "***"

_SECRET_NAME_RE = re.compile(
    r"PASSWORD|PASSWD|PWD|SECRET|CREDENTIAL|TOKEN"
    r"|(?:^|[_.\-])(?:PASS|AUTH|(?:API|PRIVATE[_.\-]?)?KEYS?)(?:$|[_.\-])",
    re.IGNORECASE)
# A name that holds metadata about a credential, not the credential itself.
_NOT_SECRET_SUFFIX_RE = re.compile(r"[_.\-](?:TTL|PATH|FILE|DIR|URL|NAME|ID)$", re.IGNORECASE)
_API_VERSION_RE = re.compile(r"(?:[a-z0-9.\-]+/)?v\d+(?:(?:alpha|beta)\d+)?")
_LAST_APPLIED = "kubectl.kubernetes.io/last-applied-configuration"


def secret_name(name: str) -> bool:
    name = name.strip().strip("\"'")
    return _SECRET_NAME_RE.search(name) is not None and _NOT_SECRET_SUFFIX_RE.search(name) is None


def _apply(text: str, spans: list) -> str:
    if not spans:
        return text
    spans.sort()
    out, pos = [], 0
    for start, end, repl in spans:
        if start < pos:
            continue
        out.append(text[pos:start])
        out.append(repl)
        pos = end
    out.append(text[pos:])
    return "".join(out)


# --- JSON --------------------------------------------------------------------------------------
# A string never spans a line in JSON, so a stray quote on an earlier line cannot shift the
# string parity of the object below it. A string cut off by a newline or the end of the text is
# read as an unterminated string and masks to its end.
_JSON_TOKEN_RE = re.compile(r'"(?:[^"\\\n]|\\[^\n])*"?|[{}\[\]:,\n]')


def _string_content(tok: str):
    """(content start, content end) of a string token, relative to the token. The token regex
    consumes every escape, so a token that ends in a quote is closed."""
    return 1, len(tok) - 1 if len(tok) > 1 and tok.endswith('"') else len(tok)


def _decode(tok: str) -> str:
    try:
        return json.loads(tok)
    except ValueError:
        return tok.strip('"')


class _Frame:
    __slots__ = (
        "api",
        "data",
        "data_children",
        "env_ctx",
        "is_obj",
        "key",
        "kind",
        "name",
        "parent",
        "value",
    )

    def __init__(self, is_obj, parent, key, env_ctx):
        self.is_obj = is_obj
        self.parent = parent      # parent _Frame or None
        self.key = key            # the key this container is the value of
        self.env_ctx = env_ctx    # an object here may be an env entry
        self.kind = self.api = self.name = self.value = None
        self.data = []            # spans of string values in data/stringData
        self.data_children = []   # value spans when this container is a data map


def _json_spans(text: str) -> list:
    spans = []
    stack = []
    key = None             # last key token seen in the current object
    expect_key = True
    # True while the current line holds only JSON tokens and whitespace. A top-level object after
    # other text (``const opts = [{...}]``) is source code, not kubectl output.
    line_json = True
    prev_end = 0

    def owner_kind(frame):
        # The object that owns the array this object sits in, e.g. a SecretList.
        arr = frame.parent
        if arr is not None and not arr.is_obj and arr.parent is not None:
            return arr.parent.kind
        return None

    def close(frame):
        """Decide a container when it closes or when the text ends inside it."""
        parent = frame.parent
        if frame.is_obj:
            if frame.data and (frame.kind == "Secret" or (
                    frame.kind is None and (frame.api or owner_kind(frame) == "SecretList"))):
                spans.extend(frame.data)
            if frame.env_ctx and frame.name is not None and frame.value is not None \
                    and secret_name(frame.name):
                spans.append(frame.value)
            # A data map's values belong to the object that holds the map.
            if parent is not None and parent.is_obj and frame.key in ("data", "stringData"):
                parent.data.extend(frame.data_children)

    for m in _JSON_TOKEN_RE.finditer(text):
        tok = m.group()
        c = tok[0]
        if line_json and not text[prev_end:m.start()].isspace() and m.start() > prev_end:
            line_json = False
        prev_end = m.end()
        if c == "\n":
            line_json = True
            continue
        top = stack[-1] if stack else None
        if c == "{" or c == "[":
            if top is None:
                env_ctx = line_json
            elif top.is_obj:
                env_ctx = key == "env"
            else:  # an element of an array
                env_ctx = top.env_ctx
            frame = _Frame(c == "{", top, key if top is not None and top.is_obj else None, env_ctx)
            stack.append(frame)
            expect_key = frame.is_obj
            key = None
        elif c == "}" or c == "]":
            if stack:
                frame = stack.pop()
                close(frame)
            top = stack[-1] if stack else None
            expect_key = False
        elif c == ":":
            expect_key = False
        elif c == ",":
            expect_key = top is not None and top.is_obj
        elif c == '"':
            if top is not None and top.is_obj and expect_key:
                key = _decode(tok)
            elif top is not None and top.is_obj:
                a, b = _string_content(tok)
                start, end = m.start() + a, m.start() + b
                if key == "kind":
                    top.kind = _decode(tok)
                elif key == "apiVersion":
                    if _API_VERSION_RE.fullmatch(_decode(tok)):
                        top.api = True
                elif key == "name":
                    top.name = _decode(tok)
                elif key == "value":
                    top.value = (start, end, MASK)
                elif key == _LAST_APPLIED:
                    spans.extend(_last_applied_span(tok, m.start(), start, end))
                if top.parent is not None and top.key in ("data", "stringData") and top.parent.is_obj:
                    top.data_children.append((start, end, MASK))
    while stack:  # cut-off text: decide every open object now
        close(stack.pop())
    return spans


def _last_applied_span(tok: str, offset: int, start: int, end: int) -> list:
    """The annotation holds the object as escaped JSON: redact it and re-escape it."""
    try:
        inner = json.loads(tok)
    except ValueError:
        return [(start, end, MASK)]  # cut off: its content cannot be read, so fail closed
    redacted = redact_kubernetes(inner)
    if redacted == inner:
        return []
    return [(offset, offset + len(tok), json.dumps(redacted, ensure_ascii=False))]


# --- YAML --------------------------------------------------------------------------------------
_YAML_KEY_RE = re.compile(r"""(-[ \t]+)?("(?:[^"\\]|\\.)*"|'(?:[^']|'')*'|[^\s#"'][^:#]*?):(?:[ \t]+|$)""")
_DOC_SEP_RE = re.compile(r"(?:---|\.\.\.)(?:[ \t].*)?")
_FLOW_VALUE_RE = re.compile(r"(:[ \t]*)([^,{}]*)")


def _plain_scalar(s: str) -> str:
    s = s.split(" #", 1)[0].strip()
    return s.strip("\"'")


class _Map:
    __slots__ = ("api", "col", "data", "kind", "name", "parent_kind", "value")

    def __init__(self, col, parent_kind):
        self.col = col
        self.kind = self.api = self.name = self.value = None
        self.data = []
        self.parent_kind = parent_kind


def _value_span(line_off: int, body: str, vstart: int):
    """Span of a scalar value on its key line: inside quotes, else up to a trailing comment."""
    value = body[vstart:]
    if value[:1] in "\"'":
        q = value[0]
        close = value.find(q, 1)
        end = vstart + (close if close > 0 else len(value))
        return line_off + vstart + 1, line_off + end
    cut = value.find(" #")
    end = vstart + (cut if cut >= 0 else len(value.rstrip()))
    return line_off + vstart, line_off + end


def _yaml_spans(text: str) -> list:
    spans = []
    lines = text.split("\n")
    offs, pos = [], 0
    for line in lines:
        offs.append(pos)
        pos += len(line) + 1
    bodies = [ln.removesuffix("\r") for ln in lines]
    indents = [len(b) - len(b.lstrip(" \t")) for b in bodies]

    def deeper_block(i, col):
        """Spans of every line after ``i`` indented past ``col`` (blank lines included)."""
        out, j = [], i + 1
        while j < len(bodies):
            b = bodies[j]
            if b.strip():
                if indents[j] <= col:
                    break
                out.append((offs[j] + indents[j], offs[j] + len(b), MASK))
            j += 1
        return out, j

    stack = []

    def close(mp):
        if mp.data and (mp.kind == "Secret" or (mp.kind is None and (mp.api or mp.parent_kind == "SecretList"))):
            spans.extend(mp.data)
        if mp.name is not None and mp.value is not None and secret_name(mp.name):
            spans.extend(mp.value)

    i, n = 0, len(bodies)
    while i < n:
        body = bodies[i]
        stripped = body.strip()
        if not stripped or stripped.startswith("#"):
            i += 1
            continue
        if _DOC_SEP_RE.fullmatch(body):
            while stack:
                close(stack.pop())
            i += 1
            continue
        ind = indents[i]
        m = _YAML_KEY_RE.match(body, ind)
        if not m:
            i += 1
            continue
        dash = m.group(1)
        col = ind + (len(dash) if dash else 0)
        while stack and (stack[-1].col > col or (dash and stack[-1].col >= col)):
            close(stack.pop())
        if not stack or stack[-1].col != col:
            parent_kind = stack[-1].kind if stack else None
            stack.append(_Map(col, parent_kind))
        mp = stack[-1]
        key = m.group(2).strip("\"'")
        vstart = m.end()
        rest = body[vstart:]
        scalar = _plain_scalar(rest)
        nxt = i + 1
        if key == "kind":
            mp.kind = scalar
            # Items of a SecretList are its children.
        elif key == "apiVersion":
            mp.api = _API_VERSION_RE.fullmatch(scalar) is not None
        elif key == "name":
            mp.name = scalar
        elif key == "value":
            if not rest.strip() or rest.lstrip()[:1] in "|>":
                block, nxt = deeper_block(i, col)
                mp.value = block
            else:
                s, e = _value_span(offs[i], body, vstart)
                block, nxt = deeper_block(i, col)
                mp.value = [(s, e, MASK)] + block
        elif key in ("data", "stringData"):
            if rest.lstrip().startswith("{"):
                for fm in _FLOW_VALUE_RE.finditer(body, vstart + rest.index("{")):
                    if fm.group(2).strip():
                        s = offs[i] + fm.start(2)
                        mp.data.append((s, s + len(fm.group(2).rstrip()), MASK))
            elif not rest.strip() or rest.strip().startswith("#"):
                nxt = _yaml_data_block(bodies, indents, offs, i, col, mp.data)
        i = nxt
    # (a key line with no case above is a plain key; its map stays on the stack)
    while stack:
        close(stack.pop())
    return spans


def _yaml_data_block(bodies, indents, offs, i, col, out) -> int:
    """Collect value spans of a ``data:`` map at ``col``; return the next line to read."""
    j, n, child = i + 1, len(bodies), -1
    while j < n:
        b = bodies[j]
        st = b.strip()
        if not st or st.startswith("#"):
            j += 1
            continue
        ind = indents[j]
        if ind <= col:
            break
        if child < 0:
            child = ind
        if ind == child:
            m = _YAML_KEY_RE.match(b, ind)
            if m and not m.group(1):
                rest = b[m.end():]
                if rest.strip() and rest.lstrip()[:1] not in "|>":
                    s, e = _value_span(offs[j], b, m.end())
                    out.append((s, e, MASK))
            else:
                out.append((offs[j] + ind, offs[j] + len(b), MASK))
        else:  # a block scalar line or the value of a key on the next line
            out.append((offs[j] + ind, offs[j] + len(b), MASK))
        j += 1
    return j


# --- Helm flow env, Go maps and kubectl describe -----------------------------------------------
_FLOW_ENV_RE = re.compile(
    r"\{[ \t]*name:[ \t]*([\"']?)([\w.\-]+)\1[ \t]*,[ \t]*value:[ \t]*([^,}\]\n]*)"
    r"|\{[ \t]*value:[ \t]*([^,}\]\n]*),[ \t]*name:[ \t]*([\"']?)([\w.\-]+)\5")
_GO_MAP_RE = re.compile(r"map\[([^\[\]\n]*)\]")
_GO_NAME_RE = re.compile(r"(?:^| )name:(\S+)")
_GO_VALUE_RE = re.compile(r"(?:^| )value:")
_GO_NEXT_KEY_RE = re.compile(r" [A-Za-z]+:")
_DESCRIBE_REF_RE = re.compile(r"<set to the key |<none>|.*\(v1:")


def _flow_spans(text: str) -> list:
    spans = []
    if "{" in text:
        for m in _FLOW_ENV_RE.finditer(text):
            if m.group(2) is not None:
                name, g = m.group(2), 3
            else:
                name, g = m.group(6), 4
            value = m.group(g).rstrip()
            if value and secret_name(name):
                s = m.start(g)
                if value[0] in "\"'" and value.endswith(value[0]) and len(value) > 1:
                    spans.append((s + 1, s + len(value) - 1, MASK))
                else:
                    spans.append((s, s + len(value), MASK))
    if "map[" in text:
        for m in _GO_MAP_RE.finditer(text):
            inner = m.group(1)
            nm = _GO_NAME_RE.search(inner)
            vm = _GO_VALUE_RE.search(inner)
            if not nm or not vm or not secret_name(nm.group(1)):
                continue
            vs = vm.end()
            nk = _GO_NEXT_KEY_RE.search(inner, vs)
            ve = nk.start() if nk else len(inner)
            if ve > vs:
                spans.append((m.start(1) + vs, m.start(1) + ve, MASK))
    return spans


def _describe_spans(text: str) -> list:
    spans, pos, env_indent = [], 0, -1
    for line in text.split("\n"):
        body = line.removesuffix("\r")
        stripped = body.lstrip(" \t")
        indent = len(body) - len(stripped)
        if env_indent >= 0 and stripped:
            if indent <= env_indent:
                env_indent = -1
            else:
                name, sep, rest = stripped.partition(":")
                value = rest.strip()
                if sep and value and " " not in name and secret_name(name) \
                        and not _DESCRIBE_REF_RE.match(value):
                    s = pos + indent + len(name) + 1 + (len(rest) - len(rest.lstrip()))
                    spans.append((s, s + len(value), MASK))
        if stripped.rstrip() == "Environment:":
            env_indent = indent
        pos += len(line) + 1
    return spans


def redact_kubernetes(text: str) -> str:
    """Mask Kubernetes Secret data values and secret-named env values."""
    spans = []
    if '"' in text and ('"data"' in text or '"stringData"' in text or '"value"' in text
                        or _LAST_APPLIED in text):
        spans += _json_spans(text)
    if "ata:" in text or "value:" in text:
        spans += _yaml_spans(text)
    if "value:" in text:
        spans += _flow_spans(text)
    if "Environment:" in text:
        spans += _describe_spans(text)
    return _apply(text, spans)
