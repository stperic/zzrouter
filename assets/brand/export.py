#!/usr/bin/env python3
"""Export the two vector masters into logos and Windows/app icons."""

from copy import deepcopy
from pathlib import Path
import shutil
import subprocess
import xml.etree.ElementTree as ET


BRAND = Path(__file__).resolve().parent
ROOT = BRAND.parent.parent
NS = "http://www.w3.org/2000/svg"
INK = "#11110f"
PAPER = "#f7f7f2"
ET.register_namespace("", NS)


def svg(width, height, title):
    root = ET.Element(f"{{{NS}}}svg", {
        "width": str(width), "height": str(height),
        "viewBox": f"0 0 {width} {height}",
        "role": "img", "aria-labelledby": "title",
    })
    ET.SubElement(root, f"{{{NS}}}title", {"id": "title"}).text = title
    return root


def artwork(master, color):
    group = deepcopy(master.find(f"{{{NS}}}g"))
    for element in group.iter():
        for attribute in ("fill", "stroke"):
            if element.get(attribute) == INK:
                element.set(attribute, color)
    return group


def write(name, root, directory=BRAND):
    ET.indent(root, space="  ")
    ET.ElementTree(root).write(directory / name, encoding="unicode")
    with (directory / name).open("a") as output:
        output.write("\n")


def console(mark, wordmark):
    directory = ROOT / "internal/server/web/console/brand"
    if not directory.is_dir():
        return
    for suffix, color, accent in (
        ("color", "#242420", "#d97757"),
        ("dark", "#faf9f5", "#d97757"),
        ("black", "#000000", "#000000"),
        ("white", "#ffffff", "#ffffff"),
    ):
        symbol = artwork(mark, color)
        symbol.findall(f"{{{NS}}}path")[-1].set("fill", accent)
        standalone = svg(64, 64, "ZZ")
        standalone.append(deepcopy(symbol))
        write(f"mark-{suffix}.svg", standalone, directory)

        letters = artwork(wordmark, color)
        if suffix in ("color", "dark"):
            separate = deepcopy(wordmark)
            separate.remove(separate.find(f"{{{NS}}}g"))
            separate.append(deepcopy(letters))
            name = "router.svg" if suffix == "color" else "router-dark.svg"
            write(name, separate, directory)

        logo = svg(300, 64, "zzRouter")
        logo.append(symbol)
        letters.set("transform", "translate(82 6) scale(0.137)")
        logo.append(letters)
        write(f"wordmark-{suffix}.svg", logo, directory)

    icon = svg(64, 64, "ZZ")
    ET.SubElement(icon, f"{{{NS}}}rect", {
        "width": "64", "height": "64", "rx": "13.5", "fill": "#c96f4d",
    })
    symbol = artwork(mark, "#242420")
    symbol.findall(f"{{{NS}}}path")[-1].set("fill", "#faf9f5")
    frame = ET.SubElement(icon, f"{{{NS}}}g", {
        "transform": "translate(5.75 5.75) scale(.82)",
    })
    frame.append(symbol)
    write("app-icon.svg", icon, directory)


def main():
    magick = shutil.which("magick")
    if magick is None:
        raise SystemExit("ImageMagick (magick) is required to export PNG icons.")

    mark = ET.parse(BRAND / "zz-mark.svg").getroot()
    wordmark = ET.parse(BRAND / "router-wordmark.svg").getroot()

    for suffix, color in (("", INK), ("-dark", PAPER)):
        logo = svg(300, 64, "zzRouter")
        logo.append(artwork(mark, color))
        word = artwork(wordmark, color)
        word.set("transform", "translate(82 6) scale(0.137)")
        logo.append(word)
        write(f"zzrouter-logo{suffix}.svg", logo)

    for name, master in (("zz-mark", mark), ("router-wordmark", wordmark)):
        reverse = deepcopy(master)
        reverse.remove(reverse.find(f"{{{NS}}}g"))
        reverse.append(artwork(master, PAPER))
        write(f"{name}-dark.svg", reverse)

    icon = svg(64, 64, "ZZ")
    ET.SubElement(icon, f"{{{NS}}}rect", {
        "width": "64", "height": "64", "rx": "12", "fill": INK,
    })
    frame = ET.SubElement(icon, f"{{{NS}}}g", {
        "transform": "translate(8 8) scale(0.75)",
    })
    frame.append(artwork(mark, PAPER))
    write("zz-icon.svg", icon)
    console(mark, wordmark)

    for path, size in (
        (BRAND / "zz-icon.png", 512),
        (ROOT / "winres/icon.png", 48),
        (ROOT / "winres/icon16.png", 16),
    ):
        subprocess.run([
            magick, "-background", "none", "-density", "768",
            str(BRAND / "zz-icon.svg"), "-resize", f"{size}x{size}",
            "-strip", f"PNG32:{path}",
        ], check=True)
        print(path.relative_to(ROOT))


if __name__ == "__main__":
    main()
