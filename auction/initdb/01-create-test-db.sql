-- O banco da aplicação é criado pelo POSTGRES_DB. Este cria o banco de
-- TESTE ao lado, porque a suíte dá TRUNCATE nas tabelas e não pode
-- encostar nos dados da aplicação.
SELECT 'CREATE DATABASE auction_test'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'auction_test')\gexec
