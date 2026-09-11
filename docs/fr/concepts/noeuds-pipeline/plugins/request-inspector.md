# request-inspector

> Plugin livré par défaut, famille « Analyse de la requête ». Retour à la [vue d'ensemble des nœuds](../index.md).

**request-inspector** relève les faits structurels. Sorties : `has_vision`, `has_reasoning`, `has_tools`, `is_streaming` (boolean), `message_count`, `input_tokens`, `max_tokens` (number). Aucune configuration. `has_vision` est le premier test d'un routage : une requête avec image doit aller vers un modèle qui voit.
