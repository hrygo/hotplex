"""Bootstrap-only exact-source editor; never included in product branches."""
from pathlib import Path

BASE = '68b9f7f0cd11e2dbf603971c4a987fa2de682fb0'


def write(path: str, text: str) -> None:
    p = Path(path)
    if p.exists():
        raise RuntimeError(f'refusing to overwrite new file: {path}')
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(text.lstrip('\n'), encoding='utf-8')


def replace(path: str, old: str, new: str, count: int = 1) -> None:
    p = Path(path)
    text = p.read_text(encoding='utf-8')
    found = text.count(old)
    if found != count:
        raise RuntimeError(f'{path}: expected {count} exact matches, found {found}: {old[:100]!r}')
    p.write_text(text.replace(old, new), encoding='utf-8')


def between(path: str, start: str, end: str, new: str) -> None:
    p = Path(path)
    text = p.read_text(encoding='utf-8')
    if text.count(start) != 1 or text.count(end) != 1:
        raise RuntimeError(f'{path}: non-unique function boundaries')
    a, b = text.index(start), text.index(end)
    if a >= b:
        raise RuntimeError(f'{path}: reversed function boundaries')
    p.write_text(text[:a] + new.lstrip('\n') + '\n\n' + text[b:], encoding='utf-8')


def main(tests, fixes) -> None:
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument('mode', choices=['tests', 'fixes'])
    mode = parser.parse_args().mode
    {'tests': tests, 'fixes': fixes}[mode]()
