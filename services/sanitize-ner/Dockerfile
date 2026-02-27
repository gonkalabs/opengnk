FROM python:3.12-slim

WORKDIR /app

# Build tools needed for packages that compile C extensions (spaCy, numpy, etc.)
RUN apt-get update && apt-get install -y --no-install-recommends \
        gcc g++ build-essential \
    && rm -rf /var/lib/apt/lists/*

# Install dependencies first (layer cache)
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Download spaCy English model
RUN python -m spacy download en_core_web_sm

# Pre-download Natasha embeddings at build time so the container starts fast
RUN python -c "from natasha import NewsEmbedding; NewsEmbedding()"

COPY main.py .

EXPOSE 8001

CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8001", "--workers", "1"]
