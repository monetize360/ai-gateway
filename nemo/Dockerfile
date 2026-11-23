FROM python:3.11-slim

# annoy (a nemoguardrails dependency) ships no linux/amd64 wheel and must be compiled,
# so g++ is installed for the pip step and removed again in the same layer.
# langchain-openai must stay on the 0.2 line. A newer release pulls langchain-core 1.x,
# which nemoguardrails 0.14.1 cannot import.
RUN apt-get update && apt-get install -y --no-install-recommends g++ \
    && pip install --no-cache-dir \
    "nemoguardrails==0.14.1" \
    "langchain-core>=0.2.35,<0.4" \
    "langchain-openai>=0.2.0,<0.3" \
    && apt-get purge -y --auto-remove g++ \
    && rm -rf /var/lib/apt/lists/*

COPY watch.sh /watch.sh
RUN chmod +x /watch.sh

EXPOSE 8084

CMD ["/watch.sh"]
