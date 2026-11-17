FROM python:3.14-slim
RUN apt-get update && apt-get upgrade -y --no-install-recommends && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY pyproject.toml ./
COPY kubecost_engine ./kubecost_engine
RUN pip install --no-cache-dir ".[runtime]" && rm -rf /usr/local/lib/python3.14/site-packages/pip* && useradd -u 65532 -r engine
USER 65532
ENV PORT=8000
EXPOSE 8000
HEALTHCHECK CMD python -c "import urllib.request,os;urllib.request.urlopen('http://127.0.0.1:'+os.environ.get('PORT','8000')+'/healthz')"
CMD ["python", "-m", "kubecost_engine.server"]
