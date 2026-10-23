FROM python:3.14-slim
WORKDIR /app
COPY pyproject.toml ./
COPY kubecost_engine ./kubecost_engine
RUN pip install --no-cache-dir . && useradd -u 65532 -r engine
USER 65532
ENV PORT=8000
EXPOSE 8000
HEALTHCHECK CMD python -c "import urllib.request,os;urllib.request.urlopen('http://127.0.0.1:'+os.environ.get('PORT','8000')+'/healthz')"
CMD ["python", "-m", "kubecost_engine.server"]
