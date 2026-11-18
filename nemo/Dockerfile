FROM python:3.11-slim

# langchain-openai must stay on the 0.2 line. A newer release pulls langchain-core 1.x,
# which nemoguardrails 0.14.1 cannot import.
RUN pip install --no-cache-dir \
    "nemoguardrails==0.14.1" \
    "langchain-core>=0.2.35,<0.4" \
    "langchain-openai>=0.2.0,<0.3"

COPY watch.sh /watch.sh
RUN chmod +x /watch.sh

EXPOSE 8084

CMD ["/watch.sh"]
