"""Rich message (блоки Telegram) → Markdown для агента.

У rich-сообщений поле message пустое, весь текст живёт в блоках. Без этого
конвертера агент видел бы такие сообщения пустыми.
"""

from __future__ import annotations

from telethon.tl import types

INLINE = {
    types.TextBold: "**{}**",
    types.TextItalic: "*{}*",
    types.TextFixed: "`{}`",
    types.TextStrike: "~~{}~~",
    types.TextMarked: "=={}==",
    types.TextUnderline: "{}",
    types.TextSubscript: "{}",
    types.TextSuperscript: "{}",
}


def text(node) -> str:
    """RichText → строка с markdown-разметкой."""
    if node is None or isinstance(node, types.TextEmpty):
        return ""
    if isinstance(node, str):
        return node
    if isinstance(node, types.TextPlain):
        return node.text
    if isinstance(node, types.TextConcat):
        return "".join(text(t) for t in node.texts)
    for cls, pattern in INLINE.items():
        if isinstance(node, cls):
            inner = text(node.text)
            return pattern.format(inner) if inner.strip() else inner
    if isinstance(node, types.TextUrl):
        return f"[{text(node.text)}]({node.url})"
    if isinstance(node, types.TextEmail):
        return f"[{text(node.text)}](mailto:{node.email})"
    if isinstance(node, types.TextPhone):
        return f"[{text(node.text)}](tel:{node.phone})"
    if isinstance(node, types.TextImage):
        return ""
    # незнакомый тип из будущих слоёв: берём, что похоже на текст
    inner = getattr(node, "text", None) or getattr(node, "texts", None)
    if isinstance(inner, list):
        return "".join(text(t) for t in inner)
    return text(inner) if inner is not None else ""


def _items(items, ordered: bool, indent: str) -> list[str]:
    lines = []
    for n, item in enumerate(items, 1):
        mark = f"{getattr(item, 'num', None) or n}." if ordered else "-"
        if hasattr(item, "blocks"):
            nested = blocks(item.blocks, indent + "  ").split("\n")
            first, rest = (nested[0], nested[1:]) if nested else ("", [])
            lines.append(f"{indent}{mark} {first.strip()}")
            lines.extend(rest)
        else:
            lines.append(f"{indent}{mark} {text(item.text)}")
    return lines


def _table(block) -> str:
    rows = []
    for i, row in enumerate(block.rows):
        cells = [text(c.text).replace("|", "\\|").replace("\n", " ") for c in row.cells]
        rows.append("| " + " | ".join(cells) + " |")
        if i == 0:
            rows.append("|" + "---|" * len(cells))
    title = text(getattr(block, "title", None))
    return (f"**{title}**\n" if title else "") + "\n".join(rows)


def block(b, indent: str = "") -> str:
    name = type(b).__name__
    if name.startswith("PageBlockHeading"):
        level = int(name[-1]) if name[-1].isdigit() else 2
        return "#" * level + " " + text(b.text)
    if isinstance(b, (types.PageBlockTitle, types.PageBlockHeader)):
        return "# " + text(b.text)
    if isinstance(b, (types.PageBlockSubtitle, types.PageBlockSubheader)):
        return "## " + text(b.text)
    if isinstance(b, types.PageBlockParagraph):
        return indent + text(b.text)
    if isinstance(b, types.PageBlockPreformatted):
        return f"```{getattr(b, 'language', '') or ''}\n{text(b.text)}\n```"
    if isinstance(b, (types.PageBlockBlockquote, types.PageBlockPullquote)):
        body = text(b.text)
        return "\n".join("> " + line for line in body.split("\n"))
    if isinstance(b, types.PageBlockDivider):
        return "---"
    if isinstance(b, types.PageBlockList):
        return "\n".join(_items(b.items, False, indent))
    if isinstance(b, types.PageBlockOrderedList):
        return "\n".join(_items(b.items, True, indent))
    if isinstance(b, types.PageBlockTable):
        return _table(b)
    if isinstance(b, types.PageBlockDetails):
        return f"**{text(b.title)}**\n" + blocks(b.blocks, indent)
    if isinstance(b, (types.PageBlockPhoto, types.PageBlockVideo, types.PageBlockAudio)):
        caption = text(getattr(getattr(b, "caption", None), "text", None))
        kind = name.replace("PageBlock", "").lower()
        return f"[{kind}{': ' + caption if caption else ''}]"
    # запасной путь для незнакомых блоков
    if hasattr(b, "blocks"):
        return blocks(b.blocks, indent)
    if hasattr(b, "text"):
        return indent + text(b.text)
    return f"[{name}]"


def blocks(items, indent: str = "") -> str:
    return "\n\n".join(part for part in (block(b, indent) for b in items or []) if part.strip())


def to_markdown(rich) -> str:
    try:
        return blocks(rich.blocks)
    except Exception as exc:  # noqa: BLE001 — лучше пометка, чем падение всего чтения
        return f"[rich-сообщение не удалось разобрать: {type(exc).__name__}]"
