"""
sanitize-ner: lightweight NER sidecar for opengnk.

Exposes a single endpoint:
  POST /classify   {"text": "..."}
  → {"spans": [{"start": 0, "end": 10, "label": "PER", "text": "Иван Иванов"}]}

Uses:
  - Natasha NewsNERTagger for Russian text  (PER, ORG, LOC)
  - spaCy en_core_web_sm  for English text  (PERSON, ORG, GPE, MONEY, DATE, …)

Deliberately avoids pymorphy2 / MorphVocab which is broken on Python 3.12.
The neural NER tagger alone gives good recall for the entity types we care about.
"""

from __future__ import annotations

import logging
import re
from contextlib import asynccontextmanager
from typing import Any

import spacy
from fastapi import FastAPI
from natasha import (
    NewsEmbedding,
    NewsNERTagger,
    Segmenter,
    Doc,
)
from pydantic import BaseModel

log = logging.getLogger("sanitize-ner")
logging.basicConfig(level=logging.INFO)

# ---------------------------------------------------------------------------
# Models — loaded once at startup
# ---------------------------------------------------------------------------

class _Models:
    segmenter: Segmenter
    emb: NewsEmbedding
    ner_tagger: NewsNERTagger
    spacy_nlp: Any  # spacy Language


_m = _Models()


@asynccontextmanager
async def lifespan(_app: FastAPI):
    log.info("Loading Natasha models…")
    _m.segmenter = Segmenter()
    _m.emb = NewsEmbedding()
    _m.ner_tagger = NewsNERTagger(_m.emb)
    log.info("Loading spaCy en_core_web_sm…")
    _m.spacy_nlp = spacy.load("en_core_web_sm")
    log.info("All models ready.")
    yield


app = FastAPI(title="sanitize-ner", lifespan=lifespan)

# ---------------------------------------------------------------------------
# Schema
# ---------------------------------------------------------------------------

class ClassifyRequest(BaseModel):
    text: str


class Span(BaseModel):
    start: int
    end: int
    label: str
    text: str


class ClassifyResponse(BaseModel):
    spans: list[Span]


# ---------------------------------------------------------------------------
# NER helpers
# ---------------------------------------------------------------------------

_CYRILLIC = re.compile(r"[а-яёА-ЯЁ]")


def _has_cyrillic(text: str) -> bool:
    return bool(_CYRILLIC.search(text))


def _natasha_spans(text: str) -> list[Span]:
    """Run Natasha neural NER on Russian text. Returns PER, ORG, LOC spans."""
    doc = Doc(text)
    doc.segment(_m.segmenter)
    doc.tag_ner(_m.ner_tagger)

    spans: list[Span] = []
    for span in doc.spans:
        spans.append(Span(
            start=span.start,
            end=span.stop,
            label=span.type,
            text=span.text,
        ))
    return _deduplicate(spans)


# spaCy labels we consider sensitive
_SPACY_SENSITIVE = {"PERSON", "ORG", "GPE", "LOC", "MONEY", "CARDINAL", "DATE", "TIME", "NORP"}


def _spacy_spans(text: str) -> list[Span]:
    """Run spaCy NER on English text."""
    doc = _m.spacy_nlp(text)
    spans: list[Span] = []
    for ent in doc.ents:
        if ent.label_ in _SPACY_SENSITIVE:
            spans.append(Span(
                start=ent.start_char,
                end=ent.end_char,
                label=ent.label_,
                text=ent.text,
            ))
    return spans


def _deduplicate(spans: list[Span]) -> list[Span]:
    """Remove overlapping spans, keeping the longest."""
    spans = sorted(spans, key=lambda s: (s.start, -(s.end - s.start)))
    result: list[Span] = []
    last_end = -1
    for s in spans:
        if s.start >= last_end:
            result.append(s)
            last_end = s.end
    return result


# ---------------------------------------------------------------------------
# Endpoints
# ---------------------------------------------------------------------------

@app.get("/health")
def health():
    return {"status": "ok"}


@app.post("/classify", response_model=ClassifyResponse)
def classify(req: ClassifyRequest) -> ClassifyResponse:
    text = req.text
    if not text.strip():
        return ClassifyResponse(spans=[])

    if _has_cyrillic(text):
        spans = _natasha_spans(text)
    else:
        spans = _spacy_spans(text)

    return ClassifyResponse(spans=spans)
