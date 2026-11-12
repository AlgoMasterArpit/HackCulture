"""Turn the rendered slide images into the PDF and PowerPoint decks.

Render the slides first (one PNG per <section class="slide"> of docs/deck/index.html,
named slide-01.png, slide-02.png, ...), then:

    python docs/deck/build.py <folder with the PNGs>
"""
import pathlib
import sys

from PIL import Image
from pptx import Presentation
from pptx.util import Emu

DOCS = pathlib.Path(__file__).resolve().parent.parent


def main(folder):
    slides = sorted(pathlib.Path(folder).glob("slide-*.png"))
    if not slides:
        raise SystemExit(f"no slide-*.png in {folder}")

    images = [Image.open(s).convert("RGB") for s in slides]
    images[0].save(DOCS / "pitch-deck.pdf", save_all=True, append_images=images[1:], resolution=150)

    deck = Presentation()
    deck.slide_width, deck.slide_height = Emu(12192000), Emu(6858000)  # 16:9
    blank = deck.slide_layouts[6]
    for path in slides:
        slide = deck.slides.add_slide(blank)
        slide.shapes.add_picture(str(path), 0, 0, width=deck.slide_width, height=deck.slide_height)
    deck.save(DOCS / "pitch-deck.pptx")
    print(f"{len(slides)} slides -> docs/pitch-deck.pdf, docs/pitch-deck.pptx")


if __name__ == "__main__":
    main(sys.argv[1] if len(sys.argv) > 1 else ".playwright-mcp/deck")
