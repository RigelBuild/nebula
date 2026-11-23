"""Kubernetes secret values in tool output (RIG-11725).

A Secret's ``data``/``stringData`` values and an env ``value`` under a secret-named ``name``
reach the model through kubectl output. Their keys are neutral (``connection``, ``value``), so
the keyword assignment passes in agent.redact never see them.

Each pass collects ``(start, end, replacement)`` spans and the text is rebuilt once. A ``None``
replacement takes the caller's mask. Every pass is linear in the text: one tokenizer pass for
JSON (its nesting capped at ``_MAX_DEPTH``), one pass over lines for YAML and ``kubectl
describe``, and regexes whose repeated parts cannot overlap.

kubectl sorts keys, so ``data`` comes before ``kind``, and output cut off by ``head -c`` ends
inside it. An object is decided when it closes, or at the end of the text:

* ``kind: Secret`` masks; any other complete kind keeps.
* No kind (or a kind cut off mid-word) masks when the text ends inside the object and it has a
  Kubernetes ``apiVersion``. A complete object with no kind keeps its own values.
* A list item with no kind hands its values to the list, which masks them when its kind is
  ``SecretList``, or when its kind is still unknown at the end of the text.
"""

import json
import re

_SECRET_NAME_RE = re.compile(
    r"PASSWORD|PASSWD|PWD|SECRET|CREDENTIAL|TOKEN"
    r"|(?:^|[_.\-])(?:PASS|AUTH|(?:API|PRIVATE[_.\-]?)?KEYS?)(?:$|[_.\-])",
    re.IGNORECASE)
# Exact names of settings about a credential, not the credential itself: counters, limits,
# switches and locations. A suffix alone never unmasks a name (``API_TOKENS`` is a token list).
# ``*_SECRET_ID`` is not here, as a Vault AppRole secret_id is live.
_NOT_SECRET_NAMES = frozenset((
    "MAX_TOKENS", "MIN_TOKENS", "TOTAL_TOKENS", "NUM_TOKENS", "MAX_NEW_TOKENS",
    "MAX_INPUT_TOKENS", "MAX_OUTPUT_TOKENS", "MAX_COMPLETION_TOKENS", "PROMPT_TOKENS",
    "COMPLETION_TOKENS", "TOKENIZERS_PARALLELISM", "TOKEN_TTL", "TOKEN_URL",
    "PASSWORD_MIN_LENGTH", "AUTH_ENABLED", "AUTH_DISABLED", "AUTH_MODE", "AUTH_TYPE",
    "SECRET_ROTATION_DAYS", "VAULT_SECRET_PATH",
))
_SEP_RE = re.compile(r"[.\-]")
_API_VERSION_RE = re.compile(r"(?:[a-z0-9.\-]+/)?v\d+(?:(?:alpha|beta)\d+)?")
_LAST_APPLIED = "kubectl.kubernetes.io/last-applied-configuration"


def _secret_name(name: str, value: str = "") -> bool:
    """Whether the value of env entry ``name`` is masked. A ``*_FILE`` value stays visible only
    when it is an absolute path."""
    name = name.strip().strip("\"'")
    if _SECRET_NAME_RE.search(name) is None:
        return False
    if name.upper().endswith(("_FILE", ".FILE", "-FILE")):
        return not value.strip().strip("\"'").startswith("/")
    # A dotted metric namespace (``llm.total_tokens``) is not part of the name.
    return _SEP_RE.sub("_", name.rpartition(".")[2]).upper() not in _NOT_SECRET_NAMES


def _apply(text: str, spans: list, mask: str) -> str:
    if not spans:
        return text
    spans.sort(key=lambda s: (s[0], s[1]))
    out, pos = [], 0
    for start, end, repl in spans:
        if start < pos:
            continue
        out.append(text[pos:start])
        out.append(mask if repl is None else repl)
        pos = end
    out.append(text[pos:])
    return "".join(out)


def _redact_last_applied(encoded: str, mask: str):
    """The annotation holds the object as escaped JSON: redact it and re-escape it. None when
    nothing changed; the mask when the value cannot be read (cut off), so it fails closed."""
    try:
        inner = json.loads(encoded)
    except ValueError:
        return mask
    if not isinstance(inner, str):
        return None
    redacted = redact_kubernetes(inner, mask)
    return None if redacted == inner else json.dumps(redacted, ensure_ascii=False)


# --- JSON --------------------------------------------------------------------------------------
# A string never spans a line in JSON, so a stray quote on an earlier line cannot shift the
# string parity of the object below it. A string cut off by a newline or the end of the text is
# read as an unterminated string and masks to its end.
_JSON_TOKEN_RE = re.compile(r'"(?:[^"\\\n]|\\[^\n])*"?|[{}\[\]:,\n]')
# Deeper nesting masks the rest of the text. Kubernetes objects are far shallower.
_MAX_DEPTH = 512
# The text before a top-level env entry on its line: nothing, or one word followed by blanks or
# ``=``/``:`` (jsonpath ``web\t[{...}]``, ``env=[{...}]``). Code (``const opts = [``) or an
# indented literal in a call or list is not kubectl output.
_ENV_PREFIX_RE = re.compile(r"(?:[\w.\-/]+(?:[ \t]+|[=:][ \t]*))?")


def _closed(tok: str) -> bool:
    # The token regex consumes every escape, so a token that ends in a quote is closed.
    return len(tok) > 1 and tok.endswith('"')


def _decode(tok: str) -> str:
    if "\\" not in tok:
        return tok[1:-1] if _closed(tok) else tok[1:]
    try:
        return json.loads(tok)
    except ValueError:
        return tok.strip('"')


class _Frame:
    __slots__ = ("api", "data", "data_owner", "deferred", "env_ctx", "is_obj", "key", "kind",
                 "name", "parent", "value")

    def __init__(self, is_obj, parent, key, env_ctx, data_owner):
        self.is_obj = is_obj
        self.parent = parent          # parent _Frame or None
        self.key = key                # the key this container is the value of
        self.env_ctx = env_ctx        # an object here may be an env entry
        self.data_owner = data_owner  # the object whose data map holds this container
        self.kind = self.api = self.name = self.value = None
        self.data = []                # spans of string values in this object's data maps
        self.deferred = []            # data spans of list items with no kind


def _list_owner(frame):
    arr = frame.parent
    if arr is not None and not arr.is_obj and arr.parent is not None and arr.parent.is_obj:
        return arr.parent
    return None


def _json_spans(text: str, mask: str) -> list:
    spans, stack = [], []
    key = None          # the last key read in the current object
    expect_key = False
    seg_start = 0       # start of the line, or end of the last top-level value on it

    def close(frame, eof):
        if not frame.is_obj:
            return
        if frame.data:
            if frame.kind == "Secret" or (frame.kind is None and eof and frame.api):
                spans.extend(frame.data)
            elif frame.kind is None:
                owner = _list_owner(frame)
                if owner is not None:
                    owner.deferred.extend(frame.data)
        if frame.deferred and (frame.kind == "SecretList" or (frame.kind is None and eof)):
            spans.extend(frame.deferred)
        # A name cut off before it is read fails closed.
        if frame.env_ctx and frame.value is not None and (
                (frame.name is None and eof) or (frame.name is not None and _secret_name(
                    frame.name, text[frame.value[0]:frame.value[1]]))):
            spans.append(frame.value)

    for m in _JSON_TOKEN_RE.finditer(text):
        tok = m.group()
        c = tok[0]
        if c == "\n":
            seg_start = m.end()
            continue
        top = stack[-1] if stack else None
        if c == "{" or c == "[":
            if len(stack) >= _MAX_DEPTH:
                spans.append((m.start(), len(text), None))
                break
            if top is None:
                env_ctx = _ENV_PREFIX_RE.fullmatch(text, seg_start, m.start()) is not None
                owner = None
            else:
                env_ctx = key == "env" if top.is_obj else top.env_ctx
                owner = top if top.is_obj and key in ("data", "stringData") else top.data_owner
            stack.append(_Frame(c == "{", top, key if top is not None and top.is_obj else None,
                                env_ctx, owner))
            expect_key = c == "{"
            key = None
        elif c == "}" or c == "]":
            if stack:
                close(stack.pop(), False)
                if not stack:
                    seg_start = m.end()
            expect_key = False
        elif c == ":":
            expect_key = False
        elif c == ",":
            expect_key = top is not None and top.is_obj
        elif top is not None:  # a string
            if top.is_obj and expect_key:
                key = _decode(tok)
                continue
            start, end = m.start() + 1, m.end() - (1 if _closed(tok) else 0)
            if top.is_obj:
                if key == "kind":
                    if _closed(tok):  # a kind cut off mid-word stays unknown
                        top.kind = _decode(tok)
                elif key == "apiVersion":
                    top.api = _API_VERSION_RE.fullmatch(_decode(tok)) is not None
                elif key == "name":
                    top.name = _decode(tok)
                elif key == "value":
                    top.value = (start, end, None)
                elif key == _LAST_APPLIED:
                    repl = _redact_last_applied(tok, mask)
                    if repl is not None:
                        spans.append((m.start(), m.end(), repl))
            if top.data_owner is not None:
                top.data_owner.data.append((start, end, None))
    while stack:  # cut-off text: decide every open object now
        close(stack.pop(), True)
    return spans


# --- YAML --------------------------------------------------------------------------------------
_YAML_KEY_RE = re.compile(r"""(-[ \t]+)?("(?:[^"\\]|\\.)*"|'(?:[^']|'')*'|[^\s#"'][^:#]*?):(?:[ \t]+|$)""")
_DOC_SEP_RE = re.compile(r"(?:---|\.\.\.)(?:[ \t].*)?")
_FLOW_VALUE_RE = re.compile(r"(:[ \t]*)([^,{}]*)")


def _plain_scalar(s: str) -> str:
    s = s.split(" #", 1)[0].strip()
    return s.strip("\"'")


def _strip_props(rest: str) -> str:
    """The value after any ``&anchor`` and ``!tag`` properties."""
    rest = rest.strip()
    i, n = 0, len(rest)
    while i < n and rest[i] in "&!":
        sp = rest.find(" ", i)
        if sp < 0:
            return ""
        i = sp
        while i < n and rest[i].isspace():
            i += 1
    return rest[i:]


class _Map:
    __slots__ = ("api", "col", "data", "deferred", "kind", "name", "parent", "value")

    def __init__(self, col, parent):
        self.col = col
        self.parent = parent
        self.kind = self.api = self.name = self.value = None
        self.data = []
        self.deferred = []


def _value_span(line_off: int, body: str, vstart: int):
    """Span of a scalar value on its key line: inside quotes, else up to a trailing comment."""
    value = body[vstart:]
    if value[:1] in "\"'":
        q = value[0]
        close = value.find(q, 1)
        end = vstart + (close if close > 0 else len(value))
        return line_off + vstart + 1, line_off + end, None
    cut = value.find(" #")
    end = vstart + (cut if cut >= 0 else len(value.rstrip()))
    return line_off + vstart, line_off + end, None


def _yaml_spans(text: str, mask: str) -> list:
    spans = []
    lines = text.split("\n")
    offs, pos = [], 0
    for line in lines:
        offs.append(pos)
        pos += len(line) + 1
    bodies = [ln.removesuffix("\r") for ln in lines]
    indents = [len(b) - len(b.lstrip(" \t")) for b in bodies]
    cut_last = not text.endswith("\n")

    def deeper_block(i, col):
        """Spans of every line after ``i`` indented past ``col`` (blank lines included)."""
        out, j = [], i + 1
        while j < len(bodies):
            b = bodies[j]
            if b.strip():
                if indents[j] <= col:
                    break
                out.append((offs[j] + indents[j], offs[j] + len(b), None))
            j += 1
        return out, j

    stack = []

    def close(mp, eof):
        # YAML has no closing bracket, so a map with an apiVersion and no kind masks either way.
        if mp.data:
            if mp.kind == "Secret" or (mp.kind is None and mp.api):
                spans.extend(mp.data)
            elif mp.kind is None and mp.parent is not None:
                mp.parent.deferred.extend(mp.data)
        if mp.deferred and (mp.kind == "SecretList" or (mp.kind is None and eof and mp.api)):
            spans.extend(mp.deferred)
        if mp.name is not None and mp.value is not None and _secret_name(
                mp.name, text[mp.value[0][0]:mp.value[0][1]] if mp.value else ""):
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
                close(stack.pop(), False)
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
            close(stack.pop(), False)
        if not stack or stack[-1].col != col:
            stack.append(_Map(col, stack[-1] if stack else None))
        mp = stack[-1]
        key = m.group(2).strip("\"'")
        vstart = m.end()
        rest = body[vstart:]
        nxt = i + 1
        if key == "kind":
            if not (cut_last and i == n - 1):  # a kind cut off mid-word stays unknown
                mp.kind = _plain_scalar(rest)
        elif key == "apiVersion":
            mp.api = _API_VERSION_RE.fullmatch(_plain_scalar(rest)) is not None
        elif key == "name":
            mp.name = _plain_scalar(rest)
        elif key == "value":
            block, nxt = deeper_block(i, col)
            if rest.strip() and rest.lstrip()[:1] not in "|>":
                block.insert(0, _value_span(offs[i], body, vstart))
            mp.value = block
        elif key in ("data", "stringData"):
            value = _strip_props(rest)
            if value.startswith("{"):
                for fm in _FLOW_VALUE_RE.finditer(body, body.index("{", vstart)):
                    if fm.group(2).strip():
                        s = offs[i] + fm.start(2)
                        mp.data.append((s, s + len(fm.group(2).rstrip()), None))
            elif not value or value.startswith("#"):
                nxt = _yaml_data_block(bodies, indents, offs, i, col, mp.data)
        elif key == _LAST_APPLIED and rest.lstrip().startswith('"'):
            value = rest.strip()
            repl = _redact_last_applied(value, mask)
            if repl is not None:
                s = offs[i] + vstart + (len(rest) - len(rest.lstrip()))
                spans.append((s, s + len(value), repl))
        i = nxt
    while stack:
        close(stack.pop(), True)
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
                    out.append(_value_span(offs[j], b, m.end()))
            else:
                out.append((offs[j] + ind, offs[j] + len(b), None))
        else:  # a block scalar line or the value of a key on the next line
            out.append((offs[j] + ind, offs[j] + len(b), None))
        j += 1
    return j


# --- Helm flow env, Go maps and kubectl describe -----------------------------------------------
_FLOW_SCALAR = r"""("(?:[^"\\\n]|\\.)*"|'(?:[^'\n]|'')*'|[^,}\]\n]*)"""
_FLOW_ENV_RE = re.compile(
    r"\{[ \t]*name:[ \t]*([\"']?)([\w.\-]+)\1[ \t]*,[ \t]*value:[ \t]*" + _FLOW_SCALAR
    + r"|\{[ \t]*value:[ \t]*" + _FLOW_SCALAR + r",[ \t]*name:[ \t]*([\"']?)([\w.\-]+)\5")
# A map runs to the next ``map[`` that opens a key (``map[name:``), not to any ``map[``: a value
# may hold ``map[`` text (``abmap[``), which must not cut the segment short.
_GO_MAP_RE = re.compile(r"map\[(?:(?!map\[(?=name:|value:))[^\r\n])*")
_GO_NAME_RE = re.compile(r"(?:^| )name:([^\s\]]+)")
_GO_VALUE_RE = re.compile(r"(?:^| )value:")
_DESCRIBE_NAME_RE = re.compile(r"([A-Za-z_][\w.\-]*):(?:[ \t]+|$)")
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
            if value and _secret_name(name, value):
                s = m.start(g)
                if value[0] in "\"'" and value.endswith(value[0]) and len(value) > 1:
                    spans.append((s + 1, s + len(value) - 1, None))
                else:
                    spans.append((s, s + len(value), None))
    if "map[" in text:
        for m in _GO_MAP_RE.finditer(text):
            seg = m.group()[4:]
            nm = _GO_NAME_RE.search(seg)
            vm = _GO_VALUE_RE.search(seg)
            if nm is None or vm is None or vm.end() == len(seg):
                continue
            vs = m.start() + 4 + vm.end()
            if _secret_name(nm.group(1), text[vs:m.end()]):
                spans.append((vs, m.end(), None))
    return spans


def _describe_spans(text: str) -> list:
    """``NAME:  value`` lines inside an ``Environment:`` block. A value that runs over several
    lines (a PEM key, a multi-line string) masks every following line up to the next
    ``NAME:`` line or the end of the block."""
    spans, pos, env_indent, in_value = [], 0, -1, False
    for line in text.split("\n"):
        body = line.removesuffix("\r")
        stripped = body.lstrip(" \t")
        lead = len(body) - len(stripped)
        indent = len(body[:lead].expandtabs(8))
        if env_indent >= 0 and stripped:
            if indent <= env_indent:
                env_indent, in_value = -1, False
            else:
                m = _DESCRIBE_NAME_RE.match(stripped)
                if m is None:
                    if in_value:
                        spans.append((pos + lead, pos + len(body), None))
                else:
                    value = stripped[m.end():].rstrip()
                    in_value = bool(value) and _secret_name(m.group(1), value) \
                        and not _DESCRIBE_REF_RE.match(value)
                    if in_value:
                        s = pos + lead + m.end()
                        spans.append((s, s + len(value), None))
        if stripped.rstrip() == "Environment:":
            env_indent, in_value = indent, False
        pos += len(line) + 1
    return spans


def redact_kubernetes(text: str, mask: str = "***") -> str:
    """Mask Kubernetes Secret data values and secret-named env values with ``mask``."""
    spans = []
    if '"' in text and ":" in text:
        spans += _json_spans(text, mask)
    if "ata:" in text or "value:" in text or _LAST_APPLIED in text:
        spans += _yaml_spans(text, mask)
    if "value:" in text:
        spans += _flow_spans(text)
    if "Environment:" in text:
        spans += _describe_spans(text)
    return _apply(text, spans, mask)
